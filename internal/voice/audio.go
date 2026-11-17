package voice

// #include <stdlib.h>
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/hraban/opus"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const playbackQueueFrames = 12

// Hold up to 10 seconds of remote audio while the completed transcript is
// classified. Ordinary RTP still uses the original 240 ms queue limit.
const pendingPlaybackFrames = 500
const localPlaybackFrames = 8
const stereoFrameBytes = stereoChannels * pcmSampleBytes

type playbackOwner uint64

const (
	playbackRemote playbackOwner = iota
	playbackPending
	playbackLocal
	playbackBlocked
)

const playbackOwnerMask = uint64(3)

func playbackState(generation uint64, owner playbackOwner) uint64 {
	return generation<<2 | uint64(owner)
}

func playbackGeneration(state uint64) uint64  { return state >> 2 }
func playbackMode(state uint64) playbackOwner { return playbackOwner(state & playbackOwnerMask) }

const captureSampleRate = 16000
const playbackSampleRate = 48000
const stereoChannels = 2
const pcmSampleBytes = 2
const captureBitrate = 24000
const maxOpusSamples = 5760
const playbackDrainTimeout = 2 * time.Second
const cueTimeout = 2 * time.Second
const cueVolume = 0.3
const playbackGain = 1.0 // Unity gain ensures native hardware AEC (vqe_0) cancellation operates cleanly without distortion or clipping
const wakeCueFrequency = 587.33
const readyCueFrequency = 880
const endCueFrequency = 440

const captureSamples = 320
const samplesPerFrame = 960
const frameDuration = 20 * time.Millisecond
const reorderDelay = 3 * frameDuration

type captureFrame struct {
	epoch, sequence uint64
	pcm             [captureSamples]int16
}

type playbackBlock struct {
	epoch      uint64
	generation uint64
	owner      playbackOwner
	used       int
	pcm        [samplesPerFrame * 2]int16
	// A marker acknowledges the preceding PCM on the next device callback.
	done chan struct{}
}

type audio struct {
	epoch            *stationEpoch
	mu               sync.Mutex
	native           *malgo.Device
	nativeContext    *malgo.AllocatedContext
	playback         chan playbackBlock
	playbackDrops    atomic.Uint64
	queueMu          sync.Mutex
	remoteQueueLimit int
	finishing        atomic.Bool
	drained          chan struct{}
	ackDrain         func()
	stopping         atomic.Bool
	playout          atomic.Uint64
	pendingOverflow  atomic.Bool
	space            chan struct{}
	inputGate        func(captureFrame)
	current          playbackBlock
	currentAt        int
	pendingDrain     chan struct{}
	lastAudioTime    atomic.Int64
}

func newAudio(epoch *stationEpoch) *audio {
	drained := make(chan struct{})
	return &audio{
		epoch:    epoch,
		playback: make(chan playbackBlock, pendingPlaybackFrames),
		space:    make(chan struct{}, 1),
		drained:  drained,
		ackDrain: sync.OnceFunc(func() { close(drained) }),
	}
}

func (a *audio) finish(ctx context.Context) error {
	if a == nil {
		return nil
	}
	mode := playbackMode(a.playout.Load())
	if mode == playbackBlocked || mode == playbackPending {
		a.flushPlayback()
		a.ackDrain()
		return nil
	}
	a.queueMu.Lock()
	a.finishing.Store(true)
	a.queueMu.Unlock()
	deadline := time.NewTimer(playbackDrainTimeout)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-a.drained:
		return context.Cause(ctx)
	case <-deadline.C:
		return fmt.Errorf("%w: playback did not finish received audio within 2 seconds", errStationAudio)
	}
}

func (a *audio) open() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.epoch.allowed() {
		return context.Cause(a.epoch.ctx)
	}
	c, err := malgo.InitContext([]malgo.Backend{malgo.BackendAlsa}, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("%w: cannot initialize ALSA playback: %w", errStationAudio, err)
	}
	a.nativeContext = c
	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 2
	cfg.SampleRate = 48000
	cfg.PeriodSizeInFrames = samplesPerFrame
	var id malgo.DeviceID
	copy(id[:], "default")
	cfg.Playback.DeviceID = id.Pointer()
	defer C.free(cfg.Playback.DeviceID)
	a.native, err = malgo.InitDevice(c.Context, cfg, malgo.DeviceCallbacks{Data: a.callback, Stop: func() {
		if !a.stopping.Load() {
			a.epoch.revoke(fmt.Errorf("%w: playback device stopped", errStationAudio))
		}
	}})
	if err != nil {
		return fmt.Errorf("%w: cannot open ALSA default PCM: %w", errStationAudio, err)
	}
	if !a.epoch.allowed() {
		return context.Cause(a.epoch.ctx)
	}
	if startErr := a.native.Start(); startErr != nil {
		return fmt.Errorf("%w: cannot start playback: %w", errStationAudio, startErr)
	}
	return nil
}

func (a *audio) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping.Store(true)
	var err error
	if a.native != nil {
		err = a.native.Stop()
		a.native.Uninit()
		a.native = nil
	}
	if a.nativeContext != nil {
		err = errors.Join(err, a.nativeContext.Uninit())
		a.nativeContext.Free()
		a.nativeContext = nil
	}
	if err != nil {
		return fmt.Errorf("%w: ALSA playback did not close cleanly: %w", errStationAudio, err)
	}
	return nil
}

// Only the device callback owns current/currentAt/pendingDrain. Producers
// invalidate them by changing the atomic generation, never by touching them.
func (a *audio) callback(output, _ []byte, frames uint32) {
	clear(output)
	if a.pendingDrain != nil {
		close(a.pendingDrain)
		a.pendingDrain = nil
	}
	state := a.playout.Load()
	if !a.blockAllowed(a.current, state) {
		a.current.used, a.currentAt = 0, 0
	}
	mode := playbackMode(state)
	if a.finishing.Load() && (mode == playbackBlocked ||
		(a.currentAt >= a.current.used && len(a.playback) == 0)) {
		a.ackDrain()
		return
	}
	if !a.epoch.allowed() || mode == playbackPending || mode == playbackBlocked {
		return
	}
	played := false
	for i := range min(int(frames), len(output)/stereoFrameBytes) {
		if !a.epoch.allowed() || a.playout.Load() != state {
			clear(output)
			return
		}
		for attempt := 0; a.currentAt >= a.current.used && attempt < cap(a.playback); attempt++ {
			select {
			case block := <-a.playback:
				a.notifySpace()
				if !a.blockAllowed(block, state) {
					continue
				}
				if block.done != nil {
					a.pendingDrain = block.done
					if !a.epoch.allowed() || a.playout.Load() != state {
						clear(output)
					} else if played {
						a.lastAudioTime.Store(time.Now().Unix())
					}
					return
				}
				a.current = block
				a.currentAt = 0
			default:
				attempt = cap(a.playback)
			}
		}
		if a.currentAt >= a.current.used {
			continue
		}
		left, right := a.current.pcm[a.currentAt*2], a.current.pcm[a.currentAt*2+1]
		binary.LittleEndian.PutUint16(output[i*4:], uint16(left))
		binary.LittleEndian.PutUint16(output[i*4+2:], uint16(right))
		a.currentAt++
		played = true
	}
	if !a.epoch.allowed() || a.playout.Load() != state {
		clear(output)
	} else if played {
		a.lastAudioTime.Store(time.Now().Unix())
	}
}

func (a *audio) blockAllowed(block playbackBlock, state uint64) bool {
	return block.epoch == a.epoch.number && block.generation == playbackGeneration(state) &&
		block.owner == playbackMode(state)
}

func (a *audio) notifySpace() {
	select {
	case a.space <- struct{}{}:
	default:
	}
}

//nolint:gocognit,nilerr // Keep capture admission ordered; cancellation is carried by the owning epoch.
func (a *audio) send(ctx context.Context, track *webrtc.TrackLocalStaticSample, input <-chan captureFrame) error {
	encoder, err := opus.NewEncoder(captureSampleRate, 1, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("%w: cannot initialize Opus encoder", errStationAudio)
	}
	if encoder.SetBitrate(captureBitrate) != nil {
		return fmt.Errorf("%w: cannot configure Opus encoder", errStationAudio)
	}
	var packet [4000]byte
	var expected uint64
	haveFrame := false
	for {
		var frame captureFrame
		select {
		case <-ctx.Done():
			return nil
		case next, ok := <-input:
			if !ok {
				return nil
			}
			frame = next
		}
		if frame.epoch != a.epoch.number || !a.epoch.allowed() || ctx.Err() != nil {
			return nil
		}
		if haveFrame && frame.sequence != expected {
			return fmt.Errorf("%w: processed capture discontinuity", errStationAudio)
		}
		expected = frame.sequence + 1
		haveFrame = true
		if a.inputGate != nil {
			a.inputGate(frame)
		}
		n, encodeErr := encoder.Encode(frame.pcm[:], packet[:])
		if encodeErr != nil {
			return fmt.Errorf("%w: Opus capture encoding failed", errStationAudio)
		}
		if !a.epoch.allowed() || ctx.Err() != nil {
			return nil
		}
		if writeSampleErr := track.WriteSample(
			media.Sample{Data: packet[:n], Duration: frameDuration},
		); writeSampleErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("audio transport send failed")
		}
	}
}

func (a *audio) queueDecoded(pcm []int16) {
	a.queueRemote(pcm, playbackGeneration(a.playout.Load()))
}

func (a *audio) queueRemote(pcm []int16, generation uint64) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	state := a.playout.Load()
	mode := playbackMode(state)
	if !a.epoch.allowed() || a.finishing.Load() || playbackGeneration(state) != generation ||
		(mode != playbackRemote && mode != playbackPending) {
		return
	}
	limit := max(playbackQueueFrames, a.remoteQueueLimit)
	if mode == playbackPending {
		limit = pendingPlaybackFrames
	} else if len(a.playback) <= playbackQueueFrames {
		// After the held prefix drains, restore ordinary RTP's short queue.
		a.remoteQueueLimit, limit = playbackQueueFrames, playbackQueueFrames
	}
	for len(pcm) > 0 {
		n := min(len(pcm), samplesPerFrame*stereoChannels)
		block := playbackBlock{epoch: a.epoch.number, generation: generation, owner: playbackRemote, used: n / stereoChannels}
		for i, s := range pcm[:n] {
			v := int32(float64(s) * playbackGain)
			if v > math.MaxInt16 {
				v = math.MaxInt16
			} else if v < math.MinInt16 {
				v = math.MinInt16
			}
			block.pcm[i] = int16(v)
		}
		pcm = pcm[n:]
		if len(a.playback) >= limit {
			a.playbackDrops.Add(1)
			if mode == playbackPending {
				a.pendingOverflow.Store(true)
			}
			continue
		}
		select {
		case a.playback <- block:
		default:
			a.playbackDrops.Add(1)
		}
	}
}

// beginPending invalidates both queued and callback-current samples before any
// potentially blocking tool or network work starts.
func (a *audio) beginPending() uint64 {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	generation := playbackGeneration(a.playout.Load()) + 1
	a.playout.Store(playbackState(generation, playbackPending))
	a.pendingOverflow.Store(false)
	a.flushLocked()
	return generation
}

func (a *audio) allowRemote(generation uint64) error {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if !a.epoch.allowed() || a.playout.Load() != playbackState(generation, playbackPending) {
		return context.Canceled
	}
	if a.pendingOverflow.Load() {
		return errors.New("completed transcript exceeded bounded remote-audio hold buffer")
	}
	// Preserve continuation while a held prefix longer than the normal queue
	// drains. Applying the 12-block limit immediately would discard new words.
	a.remoteQueueLimit = min(pendingPlaybackFrames, max(playbackQueueFrames, len(a.playback)+playbackQueueFrames))
	// The generation stays unchanged so buffered, unclassified remote PCM can
	// play without dropping the beginning of an ordinary assistant answer.
	a.playout.Store(playbackState(generation, playbackRemote))
	return nil
}

func (a *audio) claimLocal(generation uint64) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if !a.epoch.allowed() || playbackGeneration(a.playout.Load()) != generation || a.finishing.Load() {
		return false
	}
	a.playout.Store(playbackState(generation, playbackLocal))
	a.flushLocked()
	return true
}

func (a *audio) retireLocal(generation uint64) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.playout.Load() != playbackState(generation, playbackLocal) {
		return
	}
	// No timer or completed TTS goroutine may reopen the discarded /wm answer.
	// Only classification of a subsequent real user turn can allow remote PCM.
	a.playout.Store(playbackState(generation, playbackBlocked))
	a.flushLocked()
}

func (a *audio) enqueueLocal(ctx context.Context, generation uint64, block playbackBlock) error {
	block.epoch, block.generation, block.owner = a.epoch.number, generation, playbackLocal
	for {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		a.queueMu.Lock()
		if !a.epoch.allowed() || a.finishing.Load() || a.playout.Load() != playbackState(generation, playbackLocal) {
			a.queueMu.Unlock()
			return context.Canceled
		}
		if len(a.playback) < localPlaybackFrames {
			select {
			case a.playback <- block:
				a.queueMu.Unlock()
				return nil
			default:
			}
		}
		a.queueMu.Unlock()
		// Never hold queueMu while waiting: input interruption and Alice
		// revocation must be able to invalidate a blocked writer immediately.
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-a.epoch.ctx.Done():
			return context.Cause(a.epoch.ctx)
		case <-a.space:
		}
	}
}

func (a *audio) queueLocalPCM(ctx context.Context, generation uint64, pcm []byte) error {
	if len(pcm) == 0 || len(pcm) > samplesPerFrame*stereoFrameBytes || len(pcm)%stereoFrameBytes != 0 {
		return errors.New("invalid 48 kHz stereo s16le speech frame")
	}
	block := playbackBlock{used: len(pcm) / stereoFrameBytes}
	for i := range len(pcm) / pcmSampleBytes {
		block.pcm[i] = int16(binary.LittleEndian.Uint16(pcm[i*pcmSampleBytes:]))
	}
	return a.enqueueLocal(ctx, generation, block)
}

func (a *audio) drainLocal(ctx context.Context, generation uint64) error {
	done := make(chan struct{})
	if err := a.enqueueLocal(ctx, generation, playbackBlock{done: done}); err != nil {
		return err
	}
	timer := time.NewTimer(playbackDrainTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-a.epoch.ctx.Done():
			return context.Cause(a.epoch.ctx)
		case <-done:
			return a.localPlaybackCause(ctx, generation)
		case <-a.space:
			if cause := a.localPlaybackCause(ctx, generation); cause != nil {
				return cause
			}
		case <-timer.C:
			if cause := a.localPlaybackCause(ctx, generation); cause != nil {
				return cause
			}
			return fmt.Errorf("%w: local speech did not drain", errStationAudio)
		}
	}
}

func (a *audio) localPlaybackCause(ctx context.Context, generation uint64) error {
	if cause := context.Cause(a.epoch.ctx); cause != nil {
		return cause
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if a.playout.Load() != playbackState(generation, playbackLocal) {
		return context.Canceled
	}
	return nil
}

func (a *audio) flushLocked() {
	a.remoteQueueLimit = playbackQueueFrames
	for len(a.playback) > 0 {
		select {
		case <-a.playback:
		default:
		}
	}
	a.notifySpace()
}

func (a *audio) flushPlayback() {
	if a == nil {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	state := a.playout.Load()
	a.playout.Store(playbackState(playbackGeneration(state)+1, playbackMode(state)))
	a.flushLocked()
}

func (a *audio) isPlaying() bool {
	if a == nil {
		return false
	}
	mode := playbackMode(a.playout.Load())
	if mode != playbackRemote && mode != playbackLocal {
		return false // Held or discarded media is not audible activity.
	}
	return time.Now().Unix()-a.lastAudioTime.Load() < 2 || len(a.playback) > 0
}

func cuePCM(frequency float64) []int16 {
	const frames = 48000 * 150 / 1000
	const fade = 48000 * 10 / 1000
	pcm := make([]int16, frames*stereoChannels)
	for i := range frames {
		gain := cueVolume * min(1, float64(i)/fade, float64(frames-1-i)/fade)
		sample := int16(gain * math.Sin(2*math.Pi*frequency*float64(i)/playbackSampleRate) * math.MaxInt16)
		pcm[i*2], pcm[i*2+1] = sample, sample
	}
	return pcm
}

func cueWAV(pcm []int16) []byte {
	dataLen := len(pcm) * pcmSampleBytes
	totalLen := 36 + dataLen
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(totalLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 2)
	binary.LittleEndian.PutUint32(buf[24:28], 48000)
	binary.LittleEndian.PutUint32(buf[28:32], 48000*2*2)
	binary.LittleEndian.PutUint16(buf[32:34], 4)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(sample))
	}
	return buf
}

func playWakeCue(ctx context.Context) error {
	return playCue(ctx, wakeCueFrequency)
}

func playEndCue(ctx context.Context) error {
	return playCue(ctx, endCueFrequency)
}

func playCue(parent context.Context, frequency float64) error {
	ctx, cancel := context.WithTimeout(parent, cueTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	pcm := cuePCM(frequency)
	wav := cueWAV(pcm)
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/aplay",
		"-q",
		"-D",
		"default",
		"-t",
		"wav",
	)
	cmd.Stdin = bytes.NewReader(wav)
	if err := cmd.Run(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return fmt.Errorf("cannot play voice cue: %w", err)
	}
	return nil
}

type paddingOpusDepacketizer struct {
	codecs.OpusPacket
}

func (p *paddingOpusDepacketizer) Unmarshal(payload []byte) ([]byte, error) {
	// Accept padding so a depacketizer error cannot skip buffered media during purge.
	if len(payload) == 0 {
		return nil, nil
	}
	return p.OpusPacket.Unmarshal(payload)
}

//nolint:cyclop,funlen,gocognit,gocyclo // Keep RTP reorder, decoder history, PLC, and cancellation flush in one ordered receiver.
func (a *audio) receive(ctx context.Context, track *webrtc.TrackRemote) (result error) {
	codec := track.Codec()
	if codec.MimeType != webrtc.MimeTypeOpus || codec.ClockRate != 48000 || codec.Channels != 2 {
		return errors.New("unsupported remote audio codec")
	}
	decoder, err := opus.NewDecoder(playbackSampleRate, stereoChannels)
	if err != nil {
		return errors.New("cannot initialize Opus decoder")
	}
	pcm := make([]int16, maxOpusSamples*stereoChannels)
	var lastReleased uint16
	haveReleased := false
	var nextTimestamp uint32
	havePacket := false
	arrivals := make(map[uint16]time.Time)
	bufferGeneration := playbackGeneration(a.playout.Load())
	buffer := samplebuilder.New(uint16(reorderDelay/frameDuration), &paddingOpusDepacketizer{}, playbackSampleRate,
		samplebuilder.WithMaxTimeDelay(reorderDelay),
		samplebuilder.WithPacketReleaseHandler(func(packet *rtp.Packet) {
			delete(arrivals, packet.SequenceNumber)
			lastReleased = packet.SequenceNumber
			haveReleased = true
		}))
	drain := func() error {
		for sample := buffer.Pop(); sample != nil; sample = buffer.Pop() {
			if len(sample.Data) == 0 {
				continue
			}
			//nolint:nestif // Conceal small gaps or reset discontinuities before decoding the current packet.
			if havePacket {
				gap := int32(sample.PacketTimestamp - nextTimestamp)
				if gap > 0 && gap <= 3*samplesPerFrame && gap%samplesPerFrame == 0 {
					for i := int32(0); i < gap; i += samplesPerFrame {
						if decoder.DecodePLC(pcm[:samplesPerFrame*2:samplesPerFrame*2]) != nil {
							//nolint:staticcheck // Station and Opus are proper names in established user-visible diagnostics.
							return errors.New("Opus loss concealment failed")
						}
						a.queueRemote(pcm[:samplesPerFrame*2], bufferGeneration)
					}
				} else if gap != 0 {
					decoder, err = opus.NewDecoder(playbackSampleRate, stereoChannels)
					if err != nil {
						return errors.New("cannot replace Opus decoder after discontinuity")
					}
				}
			}
			n, decodeErr := decoder.Decode(sample.Data, pcm)
			if decodeErr != nil {
				return errors.New("invalid remote Opus packet")
			}
			nextTimestamp = sample.PacketTimestamp + uint32(n)
			havePacket = true
			a.queueRemote(pcm[:n*2], bufferGeneration)
		}
		return nil
	}
	defer func() {
		//nolint:errorlint // Only the exact sentinel is benign; wrapped or joined failures must remain failures.
		if context.Cause(ctx) == errProtocolHangup && a.epoch.allowed() {
			buffer.Flush()
			result = errors.Join(result, drain())
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = track.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		var deadline time.Time
		for _, arrived := range arrivals {
			if deadline.IsZero() || arrived.Add(reorderDelay).Before(deadline) {
				deadline = arrived.Add(reorderDelay)
			}
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			buffer.Flush()
			if drainErr := drain(); drainErr != nil {
				return drainErr
			}
			continue
		}
		if setReadDeadlineErr := track.SetReadDeadline(deadline); setReadDeadlineErr != nil {
			return errors.New("cannot set remote audio deadline")
		}
		if !a.epoch.allowed() || ctx.Err() != nil {
			return nil
		}
		packet, _, readRTPErr := track.ReadRTP()
		if readRTPErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if timeout, ok := errors.AsType[net.Error](readRTPErr); ok && timeout.Timeout() {
				continue
			}
			return errors.New("remote audio stream ended")
		}
		if generation := playbackGeneration(a.playout.Load()); generation != bufferGeneration {
			// Do not relabel RTP already buffered before an interruption as the
			// new turn. Continue decoding for Opus history, but drop its PCM.
			buffer.Flush()
			if drainErr := drain(); drainErr != nil {
				return drainErr
			}
			bufferGeneration = generation
		}
		if haveReleased && int16(packet.SequenceNumber-lastReleased) <= 0 {
			continue
		}
		if len(packet.Payload) == 0 && !packet.Padding {
			return errors.New("invalid remote Opus packet")
		}
		if _, pending := arrivals[packet.SequenceNumber]; !pending {
			arrivals[packet.SequenceNumber] = time.Now()
		}
		buffer.Push(packet)
		if packetDrainErr := drain(); packetDrainErr != nil {
			return packetDrainErr
		}
	}
}
