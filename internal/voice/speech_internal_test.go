package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSpeechToken = "synthetic-device-token-0123456789abcdef"

func speechFrame(pcm []byte) []byte {
	frame := make([]byte, 4+len(pcm))
	binary.LittleEndian.PutUint32(frame, uint32(len(pcm)))
	copy(frame[4:], pcm)
	return frame
}

func speechHeaders(h http.Header) {
	h.Set("Content-Type", speechStreamMediaType+"; version=1")
	h.Set("X-Audio-Format", "pcm_s16le")
	h.Set("X-Audio-Sample-Rate", "48000")
	h.Set("X-Audio-Channels", "2")
	h.Set("X-Audio-Sample-Bits", "16")
	h.Set("X-Audio-Framing", "length-prefix-le32")
	h.Set("X-Audio-Max-Frame-Bytes", "3840")
}

type speechFragmentedReader struct{ io.Reader }

func (r speechFragmentedReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), 1)]) }

func TestSpeechStreamKeepsSignedStereoPCMThroughFragmentedHTTP(t *testing.T) {
	// Independently specified signed little-endian stereo samples.
	pcm := []byte{0x2e, 0xfb, 0x2e, 0x16, 0x00, 0x80, 0xff, 0x7f}
	wire := append(speechFrame(pcm[:4]), speechFrame(pcm[4:])...)
	wire = append(wire, 0, 0, 0, 0)
	var output bytes.Buffer
	stats, err := readSpeechStream(t.Context(), speechFragmentedReader{bytes.NewReader(wire)}, time.Now(), func(p []byte) error {
		_, writeErr := output.Write(p)
		return writeErr
	})
	if err != nil || stats.Bytes != 8 || !bytes.Equal(output.Bytes(), pcm) {
		t.Fatal("framing changed PCM or lost completion", stats, err, output.Bytes())
	}
}

func TestSpeechStreamRejectsTruncationAndInvalidFrames(t *testing.T) {
	valid := speechFrame([]byte{1, 0, 2, 0})
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"no frames", []byte{0, 0, 0, 0}},
		{"partial length", []byte{4, 0}},
		{"partial PCM", []byte{4, 0, 0, 0, 1, 0}},
		{"aligned truncation", valid},
		{"invalid alignment", []byte{2, 0, 0, 0, 1, 0}},
		{"oversized", []byte{0x04, 0x0f, 0, 0}},
		{"extra after completion", append(append(append([]byte{}, valid...), 0, 0, 0, 0), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readSpeechStream(t.Context(), bytes.NewReader(tc.wire), time.Now(), func([]byte) error { return nil })
			if err == nil {
				t.Fatal("malformed speech stream accepted")
			}
		})
	}
}

func TestSpeechStreamCapsPCMBeforeAnExtraFrameReachesPlayback(t *testing.T) {
	wire := bytes.Repeat(speechFrame(make([]byte, 3840)), 2251)
	written := 0
	stats, err := readSpeechStream(t.Context(), bytes.NewReader(wire), time.Now(), func(pcm []byte) error {
		written += len(pcm)
		return nil
	})
	if err == nil || written != 8_640_000 || stats.Bytes != 8_640_000 {
		t.Fatal("stream exceeded the 45-second PCM bound", written, stats.Bytes, err)
	}
}

func TestSpeechStreamStopsOnWriterFailureAndCancellation(t *testing.T) {
	wire := append(speechFrame([]byte{1, 0, 2, 0}), 0, 0, 0, 0)
	failure := errors.New("playback lease revoked")
	stats, err := readSpeechStream(t.Context(), bytes.NewReader(wire), time.Now(), func([]byte) error { return failure })
	if err != failure || stats.Bytes != 0 {
		t.Fatal("write failure was hidden", stats, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	_, err = readSpeechStream(ctx, bytes.NewReader(wire), time.Now(), func([]byte) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled speech still wrote PCM", err)
	}
}

func TestNeuralSpeechConfigurationRejectsUnsafeEndpointsAndVoices(t *testing.T) {
	for _, config := range []neuralSpeechConfig{
		{URL: "http://192.168.1.5:8090/v1/speech", Token: testSpeechToken},
		{URL: "http://localhost:8090/v1/speech", Token: testSpeechToken},
		{URL: "https://user:pass@example.org/v1/speech", Token: testSpeechToken},
		{URL: "https://example.org/v1/speech?token=secret", Token: testSpeechToken},
		{URL: "https://example.org/v1/speech", Token: "too-short"},
		{URL: "https://example.org/v1/speech", Token: testSpeechToken + "\n"},
		{URL: "https://example.org/v1/speech", Token: testSpeechToken, Voice: "vale"},
		{URL: "https://example.org/v1/speech", Token: testSpeechToken, BargeInRMS: -1},
	} {
		if s, err := newNeuralSpeech(config); err == nil || s != nil {
			t.Fatal("unsafe configuration accepted", config.URL)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8090/v1/speech", "http://[::1]:8090/v1/speech", "https://speech.example.org/v1/speech"} {
		s, err := newNeuralSpeech(neuralSpeechConfig{URL: endpoint, Token: testSpeechToken})
		if err != nil {
			t.Fatal(err)
		}
		s.client.CloseIdleConnections()
		if s.config.WMStopSpeaking || s.config.LogTranscripts || s.config.Voice != "ru-RU-SvetlanaNeural" {
			t.Fatal("unsafe default or implicit ChatGPT voice identity")
		}
	}
}

func TestPrivateNeuralSpeechConfigIsOptionalButRejectsPermissionsAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "neural-tts.json")
	if s, err := loadNeuralSpeech(path); s != nil || err != nil {
		t.Fatal("missing optional configuration became an error", err)
	}
	content := `{"url":"http://127.0.0.1:8090/v1/speech","token":"` + testSpeechToken + `","system_prompt":"fake"}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNeuralSpeech(path); err == nil {
		t.Fatal("unknown priming configuration was accepted")
	}
	content = `{"url":"http://127.0.0.1:8090/v1/speech","token":"` + testSpeechToken + `"}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNeuralSpeech(path); err == nil {
		t.Fatal("world-readable device token was accepted")
	}
}

func TestHTTPNeuralSpeechUsesExistingAudioAndWaitsForActualDrain(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testSpeechToken || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong speech request authentication or media type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("ru-RU-SvetlanaNeural")) || !bytes.Contains(body, []byte("Проверка")) {
			t.Error("missing request text or configured voice")
		}
		speechHeaders(w.Header())
		_, _ = w.Write(speechFrame([]byte{0xd2, 0x04, 0x2e, 0xfb})) // L=1234, R=-1234.
		w.(http.Flusher).Flush()
		requests <- struct{}{}
		_, _ = w.Write([]byte{0, 0, 0, 0})
	}))
	defer server.Close()
	s, err := newNeuralSpeech(neuralSpeechConfig{URL: server.URL, Token: testSpeechToken})
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	generation := e.output.beginPending()
	if !e.output.claimLocal(generation) {
		t.Fatal("cannot claim local audio")
	}
	done := make(chan error, 1)
	go func() {
		_, speakErr := s.speak(t.Context(), e.output, generation, "Проверка")
		done <- speakErr
	}()
	<-requests
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	heard := false
	output := make([]byte, samplesPerFrame*4)
	for {
		select {
		case err := <-done:
			if err != nil || !heard {
				t.Fatal("speech returned without correct device PCM", err, heard)
			}
			return
		case <-tick.C:
			e.output.callback(output, nil, samplesPerFrame)
			if int16(binary.LittleEndian.Uint16(output)) == 1234 && int16(binary.LittleEndian.Uint16(output[2:])) == -1234 {
				heard = true
			}
		case <-deadline.C:
			t.Fatal("streaming speech did not drain")
		}
	}
}

func TestNewTurnCancelsHTTPResponseAndNoLateSpeechSurvives(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		speechHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	s, err := newNeuralSpeech(neuralSpeechConfig{URL: server.URL, Token: testSpeechToken})
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	coordinator := newTurnCoordinator(t.Context(), e.output, nil)
	defer coordinator.close()
	turn, _ := coordinator.beginInput()
	coordinator.completeInput()
	coordinator.claimLocal(turn)
	done := make(chan error, 1)
	go func() {
		_, speakErr := s.speak(turn.ctx, e.output, turn.generation, "Проверка")
		done <- speakErr
	}()
	<-started
	newTurn, _ := coordinator.beginInput()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("HTTP cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old HTTP speech was not canceled")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("proxy did not observe disconnect")
	}
	coordinator.finishLocal(turn)
	if e.output.playout.Load() != playbackState(newTurn.generation, playbackPending) {
		t.Fatal("old completion reopened PCM")
	}
}

func TestHTTPNeuralSpeechErrorsNeverLeakUpstreamBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("private-upstream-marker " + testSpeechToken))
	}))
	defer server.Close()
	s, err := newNeuralSpeech(neuralSpeechConfig{URL: server.URL, Token: testSpeechToken})
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	generation := e.output.beginPending()
	e.output.claimLocal(generation)
	_, err = s.speak(t.Context(), e.output, generation, "Проверка")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "private-upstream-marker") || strings.Contains(err.Error(), testSpeechToken) {
		t.Fatal("unsafe or absent upstream error", err)
	}
}
