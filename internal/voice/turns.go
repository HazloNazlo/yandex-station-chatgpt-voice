package voice

import (
	"context"
	"errors"
	"sync"
	"time"
)

const transcriptHoldTimeout = 60 * time.Second
const captureVADQuietFrames = 16 // 320 ms at 16 kHz / 320 samples.

type voiceTurn struct {
	id         uint64
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	waiting    bool
	timer      *time.Timer
}

// One local turn owns tool retrieval, HTTP synthesis, and queued PCM together.
// Completion never grants a previous owner permission to resume remote audio.
type turnCoordinator struct {
	mu      sync.Mutex
	parent  context.Context
	audio   *audio
	current *voiceTurn
	next    uint64
	closed  bool
	stalled func(error)
}

func newTurnCoordinator(parent context.Context, a *audio, stalled func(error)) *turnCoordinator {
	return &turnCoordinator{parent: parent, audio: a, stalled: stalled}
}

func (t *turnCoordinator) startLocked() *voiceTurn {
	if t.current != nil {
		t.current.cancel()
		if t.current.timer != nil {
			t.current.timer.Stop()
		}
	}
	t.next++
	ctx, cancel := context.WithCancel(t.parent)
	turn := &voiceTurn{id: t.next, generation: t.audio.beginPending(), ctx: ctx, cancel: cancel, waiting: true}
	t.current = turn
	turn.timer = time.AfterFunc(transcriptHoldTimeout, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.closed && t.current == turn && turn.waiting && t.parent.Err() == nil && t.stalled != nil {
			// The failure callback only cancels the peer; it must not reenter
			// this coordinator. Keep check and cancellation atomic with input.
			t.stalled(errors.New("user speech produced no completed transcript within the playback-hold deadline"))
		}
	})
	return turn
}

func (t *turnCoordinator) beginInput() (*voiceTurn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.parent.Err() != nil {
		return nil, false
	}
	if t.current != nil && t.current.waiting && t.current.ctx.Err() == nil {
		return t.current, false // Local VAD and server transcript describe the same input.
	}
	return t.startLocked(), true
}

func (t *turnCoordinator) completeInput() *voiceTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.parent.Err() != nil {
		return nil
	}
	if t.current == nil || !t.current.waiting {
		t.startLocked()
	}
	turn := t.current
	turn.waiting = false
	turn.timer.Stop()
	return turn
}

func (t *turnCoordinator) startWork(turn *voiceTurn, workers *sync.WaitGroup, work func()) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || turn == nil || t.current != turn || turn.ctx.Err() != nil {
		return false
	}
	// peer.close closes this coordinator before waiting on workers. Register
	// under the same mutex so no late worker can escape that teardown wait.
	workers.Go(work)
	return true
}

func (t *turnCoordinator) cancelIfCurrent(turn *voiceTurn, cancel context.CancelCauseFunc, cause error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || turn == nil || t.current != turn || turn.ctx.Err() != nil {
		return false
	}
	// Cancellation is nonblocking and does not reenter the coordinator. A
	// boolean check followed by cancel outside mu could terminate a new input.
	cancel(cause)
	return true
}

func (t *turnCoordinator) allowRemote(turn *voiceTurn) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || turn == nil || t.current != turn || turn.ctx.Err() != nil {
		return context.Canceled
	}
	return t.audio.allowRemote(turn.generation)
}

func (t *turnCoordinator) claimLocal(turn *voiceTurn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && turn != nil && t.current == turn && turn.ctx.Err() == nil && t.audio.claimLocal(turn.generation)
}

func (t *turnCoordinator) finishLocal(turn *voiceTurn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && turn != nil && t.current == turn {
		t.audio.retireLocal(turn.generation)
	}
}

func (t *turnCoordinator) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.current != nil {
		t.current.cancel()
		if t.current.timer != nil {
			t.current.timer.Stop()
		}
	}
}

// This is a cheap gate on hardware-VQE-processed PCM, not an ASR or a claim of
// perfect speech detection. Calibrate RMS on the actual firmware/noise floor.
// It runs before the first above-threshold frame is sent to the remote VAD.
type captureVAD struct {
	thresholdSquared int64
	active           bool
	quiet            int
}

func newCaptureVAD(rms int) *captureVAD {
	return &captureVAD{thresholdSquared: int64(rms) * int64(rms)}
}

func (v *captureVAD) observe(pcm [captureSamples]int16) bool {
	var energy int64
	for _, sample := range pcm {
		x := int64(sample)
		energy += x * x
	}
	if energy >= v.thresholdSquared*captureSamples {
		started := !v.active
		v.active, v.quiet = true, 0
		return started
	}
	if v.active {
		v.quiet++
		if v.quiet >= captureVADQuietFrames {
			v.active, v.quiet = false, 0
		}
	}
	return false
}

func (p *peer) beginUserInput() {
	if p.turns == nil {
		return
	}
	turn, started := p.turns.beginInput()
	if !started || turn == nil {
		return
	}
	p.lastActivity.Store(time.Now().Unix())
	p.report.Printf("Playback hold turn=%d", turn.id)
	if p.speech != nil && p.speech.config.WMStopSpeaking {
		p.turns.startWork(turn, &p.wg, func() {
			if turn.ctx.Err() != nil {
				return
			}
			message, err := encodeWMStopSpeaking()
			if err == nil {
				err = p.sendText(string(message))
			}
			if err != nil && p.ctx.Err() == nil {
				p.report.Print("Experimental /wm stop_speaking was not sent; local playback remains gated")
			}
		})
	}
}
