package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func constantPCM(value int16, frames int) []byte {
	pcm := make([]byte, frames*stereoFrameBytes)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(value))
	}
	return pcm
}

func assertPCMValue(t *testing.T, pcm []byte, want int16) {
	t.Helper()
	for i := 0; i < len(pcm); i += 2 {
		if got := int16(binary.LittleEndian.Uint16(pcm[i:])); got != want {
			t.Fatalf("sample %d: got %d, want %d", i/2, got, want)
		}
	}
}

func TestFlushInvalidatesCallbackCurrentAudio(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	pcm := make([]int16, samplesPerFrame*2)
	for i := range pcm {
		pcm[i] = 1000
	}
	e.output.queueDecoded(pcm)
	output := make([]byte, 480*4)
	e.output.callback(output, nil, 480)
	assertPCMValue(t, output, 1000)
	e.output.flushPlayback()
	e.output.callback(output, nil, 480)
	assertPCMValue(t, output, 0) // The unfinished 480 samples must not resume.
}

func TestPendingRemoteAudioIsHeldThenReleasedWithoutLosingPrefix(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	a := e.output
	generation := a.beginPending()
	a.queueDecoded([]int16{1100, -1100, 2200, -2200})
	output := make([]byte, 8)
	a.callback(output, nil, 2)
	assertPCMValue(t, output, 0)
	if len(a.playback) != 1 {
		t.Fatal("pending audio was consumed before classification")
	}
	if err := a.allowRemote(generation); err != nil {
		t.Fatal(err)
	}
	a.callback(output, nil, 2)
	want := []int16{1100, -1100, 2200, -2200}
	for i, value := range want {
		if got := int16(binary.LittleEndian.Uint16(output[i*2:])); got != value {
			t.Fatalf("held sample %d became %d, want %d", i, got, value)
		}
	}
}

func TestReleasingLongHeldPrefixPreservesNewRemoteContinuation(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	generation := e.output.beginPending()
	for value := int16(1); value <= 20; value++ {
		e.output.queueDecoded([]int16{value, value})
	}
	if err := e.output.allowRemote(generation); err != nil {
		t.Fatal(err)
	}
	e.output.queueDecoded([]int16{3000, 3000})
	output := make([]byte, 21*4)
	e.output.callback(output, nil, 21)
	for i := range 20 {
		if got := int16(binary.LittleEndian.Uint16(output[i*4:])); got != int16(i+1) {
			t.Fatal("held prefix lost", i, got)
		}
	}
	if got := int16(binary.LittleEndian.Uint16(output[20*4:])); got != 3000 || e.output.playbackDrops.Load() != 0 {
		t.Fatal("ordinary queue limit discarded the continuation behind held audio", got)
	}
	for range playbackQueueFrames + 1 {
		e.output.queueDecoded([]int16{4000, 4000})
	}
	if len(e.output.playback) != playbackQueueFrames || e.output.playbackDrops.Load() != 1 {
		t.Fatal("temporary hold-release capacity did not return to the normal bound")
	}
}

func TestPendingOverflowFailsClosed(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	generation := e.output.beginPending()
	for range pendingPlaybackFrames + 1 {
		e.output.queueDecoded([]int16{1000, 1000})
	}
	if len(e.output.playback) != pendingPlaybackFrames {
		t.Fatal("pending buffer is not bounded")
	}
	if err := e.output.allowRemote(generation); err == nil {
		t.Fatal("truncated held answer became audible")
	}
	output := make([]byte, 4)
	e.output.callback(output, nil, 1)
	assertPCMValue(t, output, 0)
}

func TestLocalPlaybackRejectsRemoteAndNeverReopensItOnCompletion(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	a := e.output
	generation := a.beginPending()
	a.queueDecoded([]int16{1000, 1000}) // Discard the refusal already held.
	if !a.claimLocal(generation) {
		t.Fatal("cannot claim local audio")
	}
	a.queueDecoded([]int16{2000, 2000}) // A late remote refusal must be dropped.
	if err := a.queueLocalPCM(t.Context(), generation, constantPCM(3000, 2)); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, 8)
	a.callback(output, nil, 2)
	assertPCMValue(t, output, 3000)
	a.retireLocal(generation)
	a.queueDecoded([]int16{4000, 4000})
	a.callback(output, nil, 2)
	assertPCMValue(t, output, 0)
	if err := a.queueLocalPCM(t.Context(), generation, constantPCM(5000, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("retired speech writer remained admitted", err)
	}
}

func TestLocalPlaybackBackpressureCancelsWithoutDroppingWords(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	a := e.output
	generation := a.beginPending()
	if !a.claimLocal(generation) {
		t.Fatal("cannot claim local audio")
	}
	for range localPlaybackFrames {
		if err := a.queueLocalPCM(t.Context(), generation, constantPCM(1200, samplesPerFrame)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(entered)
		done <- a.queueLocalPCM(ctx, generation, constantPCM(2300, 1))
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked PCM writer did not cancel")
	}
	if len(a.playback) != localPlaybackFrames || a.playbackDrops.Load() != 0 {
		t.Fatal("local speech used lossy RTP queue policy")
	}
}

func TestLocalDrainWaitsForSubsequentDeviceCallback(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	a := e.output
	generation := a.beginPending()
	if !a.claimLocal(generation) {
		t.Fatal("cannot claim local audio")
	}
	if err := a.queueLocalPCM(t.Context(), generation, constantPCM(1800, 1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.drainLocal(t.Context(), generation) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(a.playback) != 2 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("drain marker not queued")
		}
	}
	output := make([]byte, 8)
	a.callback(output, nil, 2) // Last sample and marker, but not yet delivered by ALSA.
	if got := int16(binary.LittleEndian.Uint16(output)); got != 1800 {
		t.Fatal("last sample was lost", got)
	}
	select {
	case err := <-done:
		t.Fatal("drain acknowledged the same callback", err)
	default:
	}
	a.callback(output, nil, 2)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("next callback did not acknowledge drain")
	}
}

func TestNewInputCancelsOldTurnAndRejectsItsLateCompletion(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	coordinator := newTurnCoordinator(t.Context(), e.output, nil)
	defer coordinator.close()
	old, started := coordinator.beginInput()
	if !started {
		t.Fatal("first input did not start a turn")
	}
	if repeated, again := coordinator.beginInput(); again || repeated != old {
		t.Fatal("local VAD and transcript duplicated a turn")
	}
	if coordinator.completeInput() != old || !coordinator.claimLocal(old) {
		t.Fatal("cannot claim first local turn")
	}
	newTurn, started := coordinator.beginInput()
	if !started || newTurn == old || old.ctx.Err() == nil {
		t.Fatal("new input did not cancel old work")
	}
	coordinator.finishLocal(old)
	if state := e.output.playout.Load(); state != playbackState(newTurn.generation, playbackPending) {
		t.Fatal("stale completion modified new playback ownership", state)
	}
	if err := coordinator.allowRemote(old); !errors.Is(err, context.Canceled) {
		t.Fatal("old turn unmuted", err)
	}
	coordinator.completeInput()
	if err := coordinator.allowRemote(newTurn); err != nil {
		t.Fatal(err)
	}
}

func TestClosedCoordinatorRefusesWorkersAfterTeardownWait(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	coordinator := newTurnCoordinator(t.Context(), e.output, nil)
	turn, _ := coordinator.beginInput()
	var workers sync.WaitGroup
	var ran atomic.Bool
	coordinator.close()
	workers.Wait()
	if coordinator.startWork(turn, &workers, func() { ran.Store(true) }) {
		t.Fatal("closed coordinator registered a worker after teardown")
	}
	workers.Wait()
	if ran.Load() {
		t.Fatal("worker escaped the teardown admission gate")
	}
}

func TestStaleTurnCannotTerminateNewInput(t *testing.T) {
	parent, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	e := newStationEpoch(parent, 1)
	defer e.revoke(context.Canceled)
	coordinator := newTurnCoordinator(parent, e.output, nil)
	defer coordinator.close()
	old, _ := coordinator.beginInput()
	coordinator.completeInput()
	newTurn, _ := coordinator.beginInput()
	if coordinator.cancelIfCurrent(old, cancel, errProtocolHangup) || parent.Err() != nil {
		t.Fatal("old farewell canceled the new input")
	}
	if !coordinator.cancelIfCurrent(newTurn, cancel, errProtocolHangup) || context.Cause(parent) != errProtocolHangup {
		t.Fatal("current farewell did not cancel its own call")
	}
}

func TestLocalDrainLosingOwnershipReturnsCancellationNotHardwareFault(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	generation := e.output.beginPending()
	e.output.claimLocal(generation)
	done := make(chan error, 1)
	go func() { done <- e.output.drainLocal(t.Context(), generation) }()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(e.output.playback) != 1 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("drain marker not queued")
		}
	}
	e.output.beginPending()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, errStationAudio) {
			t.Fatal("lease loss became a hardware failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain ignored ownership invalidation")
	}
}

func TestPendingHangupDropsInaudibleMediaInsteadOfWaitingForDrain(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	e.output.beginPending()
	e.output.queueDecoded([]int16{1200, 1200})
	if e.output.isPlaying() {
		t.Fatal("held remote media counted as audible activity")
	}
	if err := e.output.finish(t.Context()); err != nil {
		t.Fatal("unclassified media caused a drain fault", err)
	}
	if len(e.output.playback) != 0 {
		t.Fatal("held audio survived hangup")
	}
}

func TestProcessedPCMVADUsesEnergyAndQuietHysteresis(t *testing.T) {
	vad := newCaptureVAD(300)
	var quiet, loud [captureSamples]int16
	for i := range loud {
		if i%2 == 0 {
			loud[i] = 400
		} else {
			loud[i] = -400
		}
	}
	if vad.observe(quiet) || !vad.observe(loud) || vad.observe(loud) {
		t.Fatal("VAD rising edge is incorrect")
	}
	for range captureVADQuietFrames - 1 {
		if vad.observe(quiet) {
			t.Fatal("quiet PCM began speech")
		}
	}
	if vad.observe(loud) {
		t.Fatal("short pause retriggered speech")
	}
	for range captureVADQuietFrames {
		vad.observe(quiet)
	}
	if !vad.observe(loud) {
		t.Fatal("new speech after quiet period was missed")
	}
}

func TestAliceRevocationSilencesBothCurrentAndLocalQueuedPCM(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	a := e.output
	generation := a.beginPending()
	if !a.claimLocal(generation) {
		t.Fatal("cannot claim local audio")
	}
	if err := a.queueLocalPCM(t.Context(), generation, constantPCM(2900, samplesPerFrame)); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, 480*4)
	a.callback(output, nil, 480)
	assertPCMValue(t, output, 2900)
	e.revoke(errAlicePriority)
	a.callback(output, nil, 480)
	assertPCMValue(t, output, 0)
	if err := a.queueLocalPCM(t.Context(), generation, constantPCM(3000, 1)); err == nil {
		t.Fatal("Alice allowed a late local frame")
	}
}
