package voice

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (p *peer) attachAudio(a *audio, speech *neuralSpeech) {
	p.audio, p.speech = a, speech
	p.turns = newTurnCoordinator(p.ctx, a, p.fail)
	if speech != nil {
		vad := newCaptureVAD(speech.config.BargeInRMS)
		a.inputGate = func(frame captureFrame) {
			if vad.observe(frame.pcm) {
				p.beginUserInput()
			}
		}
	}
}

func (p *peer) handleMessage(data []byte) {
	// Terminal errors must still survive a previously received normal hangup.
	if p.ctx.Err() != nil && context.Cause(p.ctx) != errProtocolHangup {
		return
	}
	ev, err := decodeEvent(data)
	switch {
	case err != nil:
		p.fail(err)
		return
	case ev.err != nil:
		p.fail(ev.err)
		return
	case ev.ended:
		p.cancel(errProtocolHangup)
		return
	}
	if p.ctx.Err() != nil {
		return
	}
	unwrapped, eventType := extractWirePayload(data)
	switch eventType {
	case "chat_message_delta":
		p.userMu.Lock()
		update, transcriptErr := p.transcript.consume(unwrapped)
		activeUser := p.transcript.role == "user" || update.started || update.finished
		p.userMu.Unlock()
		if transcriptErr != nil {
			p.fail(transcriptErr)
			return
		}
		if activeUser {
			p.lastActivity.Store(time.Now().Unix())
			p.hasUserSpoken.Store(true)
		}
		if update.started {
			p.beginUserInput()
		}
		if update.finished {
			p.dispatchUserText(update.text)
		}
	case "input_audio_buffer.speech_started":
		// Compatibility observation only. /wm need not emit this public-API
		// event; the local processed-PCM gate supplies early interruption.
		p.beginUserInput()
	}
	// No automatic interpretation of public Realtime function-call events,
	// guessed role injection, or state_update-driven unmute exists here.
}

func (p *peer) localSpeech(turn *voiceTurn, text string) error {
	stats, err := p.speech.speak(turn.ctx, p.audio, turn.generation, text)
	if err != nil {
		if turn.ctx.Err() == nil && p.ctx.Err() == nil {
			p.report.Printf("Local speech failed turn=%d pcm_bytes=%d error=%v", turn.id, stats.Bytes, err)
			if errors.Is(err, errStationAudio) {
				p.fail(err)
			}
		}
		return err
	}
	p.report.Printf("Local speech drained turn=%d pcm_bytes=%d first_pcm_ms=%d elapsed_ms=%d",
		turn.id, stats.Bytes, stats.FirstPCM.Milliseconds(), stats.Elapsed.Milliseconds())
	p.lastActivity.Store(time.Now().Unix())
	return nil
}

func (p *peer) dispatchUserText(text string) {
	if p.turns == nil {
		return // Bare protocol peers in transport tests have no hardware owner.
	}
	turn := p.turns.completeInput()
	if turn == nil {
		return
	}
	if text != "" {
		p.hasUserSpoken.Store(true)
		p.lastActivity.Store(time.Now().Unix())
		if p.speech != nil && p.speech.config.LogTranscripts {
			p.report.Printf("User speech turn=%d: %q", turn.id, text)
		}
	}
	if isHangup, farewell := parseHangupCommand(text); isHangup {
		if !p.turns.claimLocal(turn) {
			return
		}
		p.turns.startWork(turn, &p.wg, func() {
			defer p.turns.finishLocal(turn)
			if farewell != "" && p.speech != nil {
				_ = p.localSpeech(turn, farewell)
			}
			p.turns.cancelIfCurrent(turn, p.cancel, errProtocolHangup)
		})
		return
	}
	if isVoice, newVoice, confirmation := parseVoiceCommand(text); isVoice {
		if !p.turns.claimLocal(turn) {
			return
		}
		p.turns.startWork(turn, &p.wg, func() {
			defer p.turns.finishLocal(turn)
			if turn.ctx.Err() != nil {
				return
			}
			if newVoice != "" {
				if err := saveVoice(newVoice); err != nil {
					confirmation = "Не удалось сохранить выбранный голос."
					newVoice = ""
				}
			}
			if confirmation != "" && p.speech != nil {
				_ = p.localSpeech(turn, confirmation)
			}
			if newVoice != "" {
				p.turns.cancelIfCurrent(turn, p.cancel, errProtocolHangup) // New native voice takes effect next call.
			}
		})
		return
	}
	if isVolume, volume, delta := parseVolumeCommand(text); isVolume {
		if !p.turns.claimLocal(turn) {
			return
		}
		p.turns.startWork(turn, &p.wg, func() {
			defer p.turns.finishLocal(turn)
			if turn.ctx.Err() != nil {
				return
			}
			var actual int
			var err error
			if volume >= 0 {
				actual, err = setStationVolume(volume)
			} else {
				actual, err = adjustStationVolume(delta)
			}
			confirmation := fmt.Sprintf("Громкость %d процентов.", actual)
			if err != nil {
				confirmation = "Не удалось изменить громкость."
			}
			if p.speech != nil {
				_ = p.localSpeech(turn, confirmation)
			}
		})
		return
	}
	if search, query := detectSearchQuery(text); search {
		if !p.turns.claimLocal(turn) {
			return
		}
		p.turns.startWork(turn, &p.wg, func() {
			defer p.turns.finishLocal(turn)
			p.report.Printf("Local tool started turn=%d", turn.id)
			ctx, cancel := context.WithTimeout(turn.ctx, 8*time.Second)
			answer, err := searchWeb(ctx, query)
			cancel()
			if turn.ctx.Err() != nil {
				return
			}
			if err != nil {
				// Never replace unavailable current data with a Wikipedia hit
				// or let the discarded /wm response become audible.
				answer = "Не удалось получить актуальные данные. Попробуйте позже."
			}
			p.report.Printf("Local tool completed turn=%d success=%t", turn.id, err == nil)
			_ = p.localSpeech(turn, answer)
		})
		return
	}
	if err := p.turns.allowRemote(turn); err != nil && !errors.Is(err, context.Canceled) {
		p.fail(err)
	}
}
