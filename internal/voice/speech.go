package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const neuralSpeechConfigPath = "/data/yandex-station-chatgpt-voice/neural-tts.json"
const speechTimeout = 45 * time.Second
const maxSpeechRunes = 2000
const maxSpeechFrameBytes = samplesPerFrame * stereoFrameBytes
const maxSpeechPCMBytes = playbackSampleRate * stereoFrameBytes * 45
const speechStreamMediaType = "application/vnd.station.pcm-stream"

// This is Station-local configuration, never part of the /wm session payload.
// The proxy token is a device credential, not a ChatGPT cookie or OAuth token.
type neuralSpeechConfig struct {
	URL            string `json:"url"`
	Token          string `json:"token"`
	Voice          string `json:"voice"`
	BargeInRMS     int    `json:"barge_in_rms"`
	WMStopSpeaking bool   `json:"experimental_wm_stop_speaking"`
	LogTranscripts bool   `json:"log_transcripts"`
}

type neuralSpeech struct {
	config neuralSpeechConfig
	client *http.Client
}

type speechStats struct {
	Bytes    int64
	FirstPCM time.Duration
	Elapsed  time.Duration
}

func loadNeuralSpeech(path string) (*neuralSpeech, error) {
	data, err := readPrivateFile(path)
	if errors.Is(err, errNoSession) {
		return nil, nil // Optional, explicitly reported as disabled by runCall.
	}
	if err != nil {
		return nil, errors.New("cannot read private neural-tts.json; use a regular client-owned file with mode 600")
	}
	defer clear(data)
	if len(data) > 8192 {
		return nil, errors.New("neural-tts.json exceeds 8 KiB")
	}
	var config neuralSpeechConfig
	if json.Unmarshal(data, &config, json.RejectUnknownMembers(true)) != nil {
		return nil, errors.New("invalid neural-tts.json; check the documented local configuration fields")
	}
	return newNeuralSpeech(config)
}

func newNeuralSpeech(config neuralSpeechConfig) (*neuralSpeech, error) {
	u, err := url.Parse(config.URL)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("neural TTS url must be an absolute HTTPS endpoint without credentials, query, or fragment")
	}
	if u.Scheme != "https" {
		ip, parseErr := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || parseErr != nil || !ip.IsLoopback() {
			return nil, errors.New("neural TTS requires HTTPS; HTTP is allowed only to a literal loopback address through a secure tunnel")
		}
	}
	if len(config.Token) < 32 || len(config.Token) > 256 {
		return nil, errors.New("neural TTS token must contain 32 to 256 printable non-whitespace ASCII bytes")
	}
	for _, b := range []byte(config.Token) {
		if b < 33 || b > 126 {
			return nil, errors.New("neural TTS token contains invalid characters")
		}
	}
	if config.Voice == "" {
		config.Voice = "ru-RU-SvetlanaNeural"
	}
	if config.Voice != "ru-RU-SvetlanaNeural" && config.Voice != "ru-RU-DmitryNeural" {
		return nil, errors.New("neural TTS voice must be ru-RU-SvetlanaNeural or ru-RU-DmitryNeural; it is not a ChatGPT voice")
	}
	if config.BargeInRMS == 0 {
		config.BargeInRMS = 300
	}
	if config.BargeInRMS < 1 || config.BargeInRMS > 32767 {
		return nil, errors.New("barge_in_rms must be from 1 to 32767 on the processed 16-bit microphone PCM")
	}
	client := newSecureHTTPClient()
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	transport.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.MaxResponseHeaderBytes = 16 << 10
	transport.MaxIdleConnsPerHost = 2
	transport.DisableCompression = true
	client.Timeout = 0 // The turn context bounds streaming and device backpressure.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &neuralSpeech{config: config, client: client}, nil
}

func boundedSpeechText(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxSpeechRunes {
		return text
	}
	return strings.TrimSpace(string(runes[:maxSpeechRunes-40])) + ". Остальной текст сокращён."
}

func newNativeNeuralSpeech() *neuralSpeech {
	return &neuralSpeech{
		config: neuralSpeechConfig{
			Voice:          "ru-RU-SvetlanaNeural",
			BargeInRMS:     300,
			WMStopSpeaking: true,
			LogTranscripts: true,
		},
		client: newSecureHTTPClient(),
	}
}

// speak performs one request only. A failure after first PCM never restarts the
// utterance, falls back to a second voice, or reopens discarded WebRTC audio.
func (s *neuralSpeech) speak(parent context.Context, a *audio, generation uint64, text string) (speechStats, error) {
	var stats speechStats
	if s == nil {
		s = newNativeNeuralSpeech()
	}
	if a == nil {
		return stats, errors.New("local speech has no Station playback owner")
	}
	text = boundedSpeechText(text)
	if text == "" || !utf8.ValidString(text) {
		return stats, errors.New("speech text must be nonempty UTF-8")
	}
	ctx, cancel := context.WithTimeout(parent, speechTimeout)
	defer cancel()
	start := time.Now()

	// Direct native Edge Neural TTS synthesis when no proxy URL is configured.
	if s.config.URL == "" {
		voice := s.config.Voice
		if voice == "" {
			voice = "ru-RU-SvetlanaNeural"
		}
		pcm, err := synthesizeEdgeTTSDirect(ctx, text, voice)
		if err != nil {
			return stats, fmt.Errorf("native edge tts failed: %w", err)
		}
		stats.FirstPCM = time.Since(start)
		frameSize := samplesPerFrame * stereoFrameBytes
		for offset := 0; offset < len(pcm); offset += frameSize {
			end := offset + frameSize
			if end > len(pcm) {
				end = len(pcm)
			}
			chunk := pcm[offset:end]
			if err := a.queueLocalPCM(ctx, generation, chunk); err != nil {
				return stats, err
			}
			stats.Bytes += int64(len(chunk))
		}
		if err := a.drainLocal(ctx, generation); err != nil {
			return stats, err
		}
		stats.Elapsed = time.Since(start)
		return stats, nil
	}
	body, err := json.Marshal(struct {
		Text  string `json:"text"`
		Voice string `json:"voice"`
	}{text, s.config.Voice})
	if err != nil {
		return stats, errors.New("cannot encode neural speech request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(body))
	if err != nil {
		return stats, errors.New("cannot prepare neural speech request")
	}
	req.Header.Set("Authorization", "Bearer "+s.config.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", speechStreamMediaType+"; version=1")
	response, err := s.client.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return stats, cause
		}
		return stats, errors.New("neural speech network or TLS request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Do not log an upstream body: it can contain credentials or transcripts.
		return stats, fmt.Errorf("neural speech proxy returned HTTP %d", response.StatusCode)
	}
	mediaType, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != speechStreamMediaType || params["version"] != "1" ||
		response.Header.Get("X-Audio-Format") != "pcm_s16le" ||
		response.Header.Get("X-Audio-Sample-Rate") != "48000" ||
		response.Header.Get("X-Audio-Channels") != "2" ||
		response.Header.Get("X-Audio-Sample-Bits") != "16" ||
		response.Header.Get("X-Audio-Framing") != "length-prefix-le32" ||
		response.Header.Get("X-Audio-Max-Frame-Bytes") != "3840" {
		return stats, errors.New("neural speech proxy returned an incompatible PCM stream")
	}
	stats, err = readSpeechStream(ctx, response.Body, start, func(pcm []byte) error {
		return a.queueLocalPCM(ctx, generation, pcm)
	})
	if err != nil {
		return stats, err
	}
	if err = a.drainLocal(ctx, generation); err != nil {
		return stats, err
	}
	stats.Elapsed = time.Since(start)
	return stats, nil
}

// Each frame is LE uint32 byte length + interleaved stereo s16le PCM. A zero
// length is a mandatory application-level completion marker. EOF alone is an
// error, even if a reverse proxy hides an upstream chunked-transfer failure.
func readSpeechStream(ctx context.Context, r io.Reader, start time.Time, writePCM func([]byte) error) (speechStats, error) {
	var stats speechStats
	var header [4]byte
	var frame [maxSpeechFrameBytes]byte
	for {
		if cause := context.Cause(ctx); cause != nil {
			return stats, cause
		}
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return stats, speechReadError(ctx, "speech stream ended without a completion marker")
		}
		n := int(binary.LittleEndian.Uint32(header[:]))
		if n == 0 {
			if stats.Bytes == 0 {
				return stats, errors.New("neural speech proxy returned no PCM")
			}
			var extra [1]byte
			if got, err := io.ReadFull(r, extra[:]); got != 0 || !errors.Is(err, io.EOF) {
				return stats, speechReadError(ctx, "speech stream has data or a transport failure after completion")
			}
			stats.Elapsed = time.Since(start)
			return stats, nil
		}
		if n < stereoFrameBytes || n > len(frame) || n%stereoFrameBytes != 0 {
			return stats, errors.New("speech stream frame has an invalid length")
		}
		if stats.Bytes+int64(n) > maxSpeechPCMBytes {
			return stats, errors.New("speech stream exceeds 45 seconds of PCM")
		}
		if _, err := io.ReadFull(r, frame[:n]); err != nil {
			return stats, speechReadError(ctx, "speech PCM frame was truncated")
		}
		if err := writePCM(frame[:n]); err != nil {
			return stats, err
		}
		if stats.Bytes == 0 {
			stats.FirstPCM = time.Since(start)
		}
		stats.Bytes += int64(n)
	}
}

func speechReadError(ctx context.Context, message string) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return errors.New(message)
}
