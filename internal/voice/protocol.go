package voice

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"uuid"
)

const localTimezoneName = "Local"
const unknownValue = "unknown"

const maxMessageBytes = 256 << 10

type event struct {
	ended bool
	err   error
}

type sessionSettings struct {
	Voice                 string
	BackendModel          string
	BackendThinkingEffort string
}

func encodeSession(now time.Time, settings sessionSettings) ([]byte, error) {
	_, east := now.Zone()
	zone := now.Location().String()
	//nolint:nestif // Preserve timezone fallback order from TZ through zoneinfo to the zone abbreviation.
	if zone == localTimezoneName {
		if tz := os.Getenv("TZ"); tz != "" {
			zone = strings.TrimPrefix(tz, ":")
		}
		if zone == localTimezoneName {
			if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
				if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
					zone = name
				}
			}
		}
		if zone == localTimezoneName {
			zone, _ = now.Zone()
		}
	}
	id := strings.ToUpper(uuid.New().String())
	voiceName := cmp.Or(settings.Voice, getSavedVoice())
	payload := map[string]any{
		"backend_reasoning_effort": "instant", "language_code": "auto",
		"requested_default_model": "", "voice": voiceName, "voice_session_id": id,
		"voice_status_request_id": id, "timezone_offset_min": -east / int(time.Minute/time.Second), "timezone": zone,
		"voice_mode": "wingman", "model_slug": "", "model_slug_advanced": "",
		"client_tools": []string{}, "history_and_training_disabled": false,
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_streaming": true, "backend_model": cmp.Or(settings.BackendModel, "gpt-6-thinking"),
		"deprecate_celsius": false,
	}
	if effort := cmp.Or(settings.BackendThinkingEffort, "extended"); effort != "disabled" {
		payload["backend_thinking_effort"] = effort
	}
	return json.Marshal(payload)
}

type wireEvent struct {
	Type    string         `json:"type"`
	Data    jsontext.Value `json:"data"`
	Payload jsontext.Value `json:"payload"`
}

//nolint:cyclop,funlen,gocognit,gocyclo // Terminal-event precedence and bounded nested-envelope decoding share one wire contract.
func decodeEvent(data []byte) (event, error) {
	if len(data) > maxMessageBytes {
		return event{}, errors.New("voice event exceeds 256 KiB")
	}
	var wire wireEvent
	if json.Unmarshal(data, &wire) != nil {
		return event{}, errors.New("invalid voice event JSON")
	}
	if wire.Type == "data_message" {
		var inner string
		if json.Unmarshal(wire.Data, &inner) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
		data = []byte(inner)
		wire = wireEvent{}
		if json.Unmarshal(data, &wire) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
	}
	if len(wire.Payload) > 0 {
		var nested wireEvent
		if json.Unmarshal(wire.Payload, &nested) == nil && nested.Type != "" {
			data = wire.Payload
			wire = nested
		}
	}
	payload := wire.Payload
	if len(payload) == 0 {
		payload = data
	}
	switch wire.Type {
	case "usage_update":
		var p struct {
			Instructions struct {
				HangUp bool `json:"hang_up"`
			} `json:"instructions"`
			Rate struct {
				Exceed   jsontext.Value `json:"exceed_limit_message"`
				Exceeded bool           `json:"exceeded"`
				Reached  bool           `json:"limit_reached"`
			} `json:"rate_limit_message"`
			Audio  *float64 `json:"audio_s"`
			Limits struct {
				Audio struct {
					Remaining *float64 `json:"remaining_seconds"`
				} `json:"audio"`
			} `json:"limits"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return event{}, errors.New("invalid usage event")
		}
		remaining := p.Audio
		if remaining == nil {
			remaining = p.Limits.Audio.Remaining
		}
		exceeded := len(p.Rate.Exceed) > 0 && string(p.Rate.Exceed) != "null" && string(p.Rate.Exceed) != "false" &&
			string(p.Rate.Exceed) != `""`
		if p.Instructions.HangUp {
			return event{ended: true}, nil
		}
		if exceeded || p.Rate.Exceeded || p.Rate.Reached || (remaining != nil && *remaining <= 0) {
			return event{err: errors.New("ChatGPT ended this call because of its usage limit")}, nil
		}
	case "tool_update":
		var p struct {
			Update string `json:"update_type"`
		}
		if json.Unmarshal(payload, &p) == nil &&
			(strings.EqualFold(p.Update, "hangup") || strings.EqualFold(p.Update, "hang_up")) {
			return event{ended: true}, nil
		}
	case "goodbye":
		var p struct {
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return event{}, errors.New("invalid goodbye event")
		}
		switch p.Reason {
		case "error", "cap_reached", "normal", "client_request", "timeout", "session_expired":
		default:
			p.Reason = unknownValue
		}
		switch p.Detail {
		case "internal_error",
			"upstream_error",
			"server_error",
			"unauthorized",
			"quota_exceeded",
			"rate_limit_exceeded",
			"invalid_session",
			"invalid_model",
			"unsupported_country":
		default:
			p.Detail = unknownValue
		}
		if p.Reason == "cap_reached" {
			return event{err: errors.New("ChatGPT ended this call because of its usage limit")}, nil
		}
		if (p.Reason == "normal" || p.Reason == "client_request") && p.Detail == unknownValue {
			return event{ended: true}, nil
		}
		return event{
			err: fmt.Errorf("ChatGPT ended this voice session (reason=%s, detail=%s)", p.Reason, p.Detail),
		}, nil
	case "error", "session.error":
		return event{err: errors.New("ChatGPT reported a voice session error")}, nil
	}
	return event{}, nil
}

// This action shape is observed in the third-party /wm SDK, not a supported
// OpenAI public contract. Sending it is explicitly opt-in; local gating does
// not depend on the server accepting or acknowledging it.
func encodeWMStopSpeaking() ([]byte, error) {
	inner, err := json.Marshal(map[string]any{
		"type": "action_request", "payload": map[string]string{"action": "stop_speaking"},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"type": "data_message", "data": string(inner)})
}

func extractWirePayload(data []byte) ([]byte, string) {
	for range 4 {
		var wire wireEvent
		if len(data) > maxMessageBytes || json.Unmarshal(data, &wire) != nil {
			return data, ""
		}
		if wire.Type == "data_message" {
			var inner string
			if json.Unmarshal(wire.Data, &inner) != nil {
				return data, ""
			}
			data = []byte(inner)
			continue
		}
		if len(wire.Payload) > 0 {
			var nested wireEvent
			if json.Unmarshal(wire.Payload, &nested) == nil && nested.Type != "" {
				data = wire.Payload
				continue
			}
		}
		return data, wire.Type
	}
	return data, ""
}

const maxTranscriptBytes = 16 << 10
const maxTranscriptParts = 64
const rememberedTranscriptIDs = 128

type transcriptPatch struct {
	P string         `json:"p"`
	O string         `json:"o"`
	V jsontext.Value `json:"v"`
}

type transcriptUpdate struct {
	started  bool
	finished bool
	id       string
	text     string
}

// The private chat_message_delta shape is bounded and fixture-tested, not a
// full ChatGPT conversation protocol implementation. Text string and {text}
// parts, append/replacement patches, and status completion are supported.
type userTranscript struct {
	id    string
	role  string
	parts []string
	seen  map[string]bool
	order []string
}

func transcriptPart(data jsontext.Value) (string, bool) {
	var text string
	if json.Unmarshal(data, &text) == nil {
		return text, true
	}
	var part struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(data, &part) == nil && part.Text != nil {
		return *part.Text, true
	}
	return "", false
}

func (u *userTranscript) remember() {
	if u.id == "" {
		return
	}
	if u.seen == nil {
		u.seen = make(map[string]bool)
	}
	if u.seen[u.id] {
		return
	}
	if len(u.order) == rememberedTranscriptIDs {
		delete(u.seen, u.order[0])
		u.order = u.order[1:]
	}
	u.seen[u.id] = true
	u.order = append(u.order, u.id)
}

func (u *userTranscript) finish() transcriptUpdate {
	update := transcriptUpdate{finished: true, id: u.id, text: strings.TrimSpace(strings.Join(u.parts, ""))}
	u.remember()
	u.role, u.parts = "", nil
	return update
}

func (u *userTranscript) bounded() error {
	var size int
	for _, text := range u.parts {
		size += len(text)
	}
	if size > maxTranscriptBytes {
		return errors.New("user transcript exceeds 16 KiB")
	}
	return nil
}

func (u *userTranscript) apply(patch transcriptPatch) (bool, error) {
	if u.role != "user" {
		return false, nil
	}
	if patch.P == "/message/status" {
		var status string
		if json.Unmarshal(patch.V, &status) != nil {
			return false, errors.New("invalid user transcript status")
		}
		return status == "finished_successfully", nil
	}
	if patch.P == "/message/content/parts" && (patch.O == "replace" || patch.O == "add") {
		var parts []jsontext.Value
		if json.Unmarshal(patch.V, &parts) != nil || len(parts) > maxTranscriptParts {
			return false, errors.New("invalid user transcript parts")
		}
		u.parts = nil
		for _, part := range parts {
			text, _ := transcriptPart(part)
			u.parts = append(u.parts, text)
		}
		return false, u.bounded()
	}
	suffix, ok := strings.CutPrefix(patch.P, "/message/content/parts/")
	if !ok {
		return false, nil
	}
	indexText, field, hasField := strings.Cut(suffix, "/")
	if hasField && field != "text" {
		return false, nil
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index >= maxTranscriptParts {
		return false, errors.New("user transcript part index is outside bounds")
	}
	text, isText := transcriptPart(patch.V)
	if !isText {
		return false, nil // Audio references and other non-text parts are not speech.
	}
	if patch.O != "append" && patch.O != "replace" && patch.O != "add" {
		return false, nil
	}
	for len(u.parts) <= index {
		u.parts = append(u.parts, "")
	}
	if patch.O == "append" {
		u.parts[index] += text
	} else {
		u.parts[index] = text
	}
	return false, u.bounded()
}

func (u *userTranscript) consume(data []byte) (transcriptUpdate, error) {
	var message struct {
		Delta transcriptPatch `json:"delta"`
	}
	if json.Unmarshal(data, &message) != nil {
		return transcriptUpdate{}, errors.New("invalid chat_message_delta")
	}
	delta := message.Delta
	var update transcriptUpdate
	if delta.O == "add" && (delta.P == "" || delta.P == "/") {
		var added struct {
			Message struct {
				ID     string `json:"id"`
				Author struct {
					Role string `json:"role"`
				} `json:"author"`
				Status  string `json:"status"`
				Content struct {
					Parts []jsontext.Value `json:"parts"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(delta.V, &added) != nil {
			return update, errors.New("invalid transcript message addition")
		}
		if added.Message.Author.Role != "user" {
			if u.role == "user" {
				// Existing /wm captures also complete the user input when the
				// assistant add arrives before a separate user status patch.
				update = u.finish()
			}
			return update, nil
		}
		if len(added.Message.ID) > 256 || len(added.Message.Content.Parts) > maxTranscriptParts {
			return update, errors.New("transcript message exceeds bounds")
		}
		if added.Message.ID != "" && u.seen[added.Message.ID] {
			return update, nil // A replay must not erase a newer active input.
		}
		if u.role == "user" && u.id == added.Message.ID && added.Message.ID != "" {
			return update, nil // Retransmitted add must not erase accumulated patches.
		}
		u.id, u.role, u.parts = added.Message.ID, "user", nil
		for _, part := range added.Message.Content.Parts {
			text, _ := transcriptPart(part)
			u.parts = append(u.parts, text)
		}
		if err := u.bounded(); err != nil {
			return update, err
		}
		update.started, update.id = true, u.id
		if added.Message.Status == "finished_successfully" {
			update = u.finish()
			update.started = true
		}
		return update, nil
	}
	patches := []transcriptPatch{delta}
	if strings.HasPrefix(strings.TrimSpace(string(delta.V)), "[") {
		if json.Unmarshal(delta.V, &patches) != nil || len(patches) > 256 {
			return update, errors.New("invalid transcript patch batch")
		}
	}
	for _, patch := range patches {
		finished, err := u.apply(patch)
		if err != nil {
			return update, err
		}
		if finished {
			update = u.finish()
		}
	}
	return update, nil
}
