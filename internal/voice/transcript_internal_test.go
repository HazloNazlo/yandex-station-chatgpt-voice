package voice

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func transcriptDelta(t *testing.T, delta any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": "chat_message_delta", "delta": delta})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func transcriptAdd(t *testing.T, id, role string, parts []any) []byte {
	return transcriptDelta(t, map[string]any{"o": "add", "v": map[string]any{"message": map[string]any{
		"id": id, "author": map[string]string{"role": role}, "content": map[string]any{"parts": parts},
	}}})
}

func TestTranscriptAssemblySupportsMixedPartsAppendAndReplacement(t *testing.T) {
	var transcript userTranscript
	update, err := transcript.consume(transcriptAdd(t, "user-1", "user", []any{map[string]string{"text": "Какая "}, "погода"}))
	if err != nil || !update.started || update.finished {
		t.Fatal(update, err)
	}
	patches := []map[string]any{
		{"o": "replace", "p": "/message/content/parts/1", "v": "погода в Калуге"},
		{"o": "append", "p": "/message/content/parts/1/text", "v": " завтра?"},
		{"o": "replace", "p": "/message/status", "v": "finished_successfully"},
	}
	update, err = transcript.consume(transcriptDelta(t, map[string]any{"o": "patch", "v": patches}))
	if err != nil || !update.finished || update.id != "user-1" || update.text != "Какая погода в Калуге завтра?" {
		t.Fatal(update, err)
	}
	update, err = transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/status", "v": "finished_successfully"}))
	if err != nil || update.finished {
		t.Fatal("status replay emitted another tool request", update, err)
	}
}

func TestTranscriptAssistantStartCompletesUserOnceAndNeverBecomesUserText(t *testing.T) {
	var transcript userTranscript
	_, _ = transcript.consume(transcriptAdd(t, "user-1", "user", []any{"Курс доллара?"}))
	update, err := transcript.consume(transcriptAdd(t, "assistant-1", "assistant", []any{"У меня нет интернета"}))
	if err != nil || !update.finished || update.text != "Курс доллара?" {
		t.Fatal(update, err)
	}
	update, err = transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/status", "o": "replace", "v": "finished_successfully"}))
	if err != nil || update.finished {
		t.Fatal("assistant status duplicated the user transcript", update, err)
	}
}

func TestTranscriptRetransmittedAdditionDoesNotErasePendingText(t *testing.T) {
	var transcript userTranscript
	addition := transcriptAdd(t, "user-1", "user", []any{"Курс "})
	_, _ = transcript.consume(addition)
	_, _ = transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/content/parts/0", "o": "append", "v": "доллара"}))
	update, err := transcript.consume(addition)
	if err != nil || update.started {
		t.Fatal("duplicate addition restarted the turn", update, err)
	}
	update, err = transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/status", "o": "replace", "v": "finished_successfully"}))
	if err != nil || update.text != "Курс доллара" {
		t.Fatal("duplicate addition erased transcript", update, err)
	}
	update, err = transcript.consume(addition)
	if err != nil || update.started || update.finished {
		t.Fatal("completed ID replay was dispatched", update, err)
	}
}

func TestCompletedTranscriptReplayCannotEraseNewUserMessage(t *testing.T) {
	var transcript userTranscript
	old := transcriptAdd(t, "user-old", "user", []any{"Старый вопрос"})
	_, _ = transcript.consume(old)
	_, _ = transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/status", "o": "replace", "v": "finished_successfully"}))
	_, _ = transcript.consume(transcriptAdd(t, "user-new", "user", []any{"Новый вопрос"}))
	if update, err := transcript.consume(old); err != nil || update.started || update.finished {
		t.Fatal("old message was re-admitted", update, err)
	}
	update, err := transcript.consume(transcriptDelta(t, map[string]any{"p": "/message/status", "o": "replace", "v": "finished_successfully"}))
	if err != nil || !update.finished || update.id != "user-new" || update.text != "Новый вопрос" {
		t.Fatal("completed replay erased active input", update, err)
	}
}

func TestTranscriptBoundsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		add   []any
		patch map[string]any
	}{
		{"oversized text", []any{strings.Repeat("x", maxTranscriptBytes+1)}, nil},
		{"part index", []any{""}, map[string]any{"p": "/message/content/parts/64/text", "o": "append", "v": "x"}},
		{"growing text", []any{strings.Repeat("x", maxTranscriptBytes)}, map[string]any{"p": "/message/content/parts/0/text", "o": "append", "v": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transcript userTranscript
			_, err := transcript.consume(transcriptAdd(t, "user-1", "user", tc.add))
			if tc.patch != nil {
				_, err = transcript.consume(transcriptDelta(t, tc.patch))
			}
			if err == nil {
				t.Fatal("unbounded transcript was accepted")
			}
		})
	}
}

func TestWMStopSpeakingUsesObservedActionEnvelope(t *testing.T) {
	data, err := encodeWMStopSpeaking()
	if err != nil {
		t.Fatal(err)
	}
	var outer struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		t.Fatal(err)
	}
	var action struct {
		Type    string `json:"type"`
		Payload struct {
			Action string `json:"action"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(outer.Data), &action); err != nil {
		t.Fatal(err)
	}
	if outer.Type != "data_message" || action.Type != "action_request" || action.Payload.Action != "stop_speaking" {
		t.Fatal("not the observed private /wm stop_speaking shape", string(data))
	}
	// This is an encoding contract test, not proof that ChatGPT accepts it.
}
