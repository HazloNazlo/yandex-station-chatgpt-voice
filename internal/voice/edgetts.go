package voice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hajimehoshi/go-mp3"
)

const (
	edgeWSSBaseURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
)

func genRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// synthesizeEdgeTTS is an alias to synthesizeEdgeTTSDirect.
func synthesizeEdgeTTS(ctx context.Context, text, voice string) ([]byte, error) {
	return synthesizeEdgeTTSDirect(ctx, text, voice)
}

// synthesizeEdgeTTSDirect connects directly to Microsoft Edge speech service over WebSocket,
// synthesizes the text into MP3 using the specified neural voice (e.g. "ru-RU-SvetlanaNeural"),
// and decodes it in memory into 48000 Hz, 16-bit, stereo PCM.
func synthesizeEdgeTTSDirect(ctx context.Context, text, voice string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty text for speech synthesis")
	}
	if voice == "" {
		voice = "ru-RU-SvetlanaNeural"
	}

	ts := getUnixTimestampWithSkew()
	token := generateSecMsGecV2(ts)
	connID := genRandHex(16)

	wsURL := fmt.Sprintf("%s?TrustedClientToken=%s&Sec-MS-GEC=%s&Sec-MS-GEC-Version=%s&ConnectionId=%s",
		edgeWSSBaseURL, trustedClientToken, token, secMsGecVersion, connID)

	header := http.Header{}
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")
	header.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	header.Set("Accept-Encoding", "gzip, deflate, br")
	header.Set("Accept-Language", "en-US,en;q=0.9")
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0")

	muid := strings.ToUpper(genRandHex(16))
	header.Set("Cookie", fmt.Sprintf("muid=%s;", muid))

	dialer := &websocket.Dialer{
		HandshakeTimeout: 8 * time.Second,
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			// Check Date header for clock skew adjustment
			if dateStr := resp.Header.Get("Date"); dateStr != "" {
				if serverTime, parseErr := http.ParseTime(dateStr); parseErr == nil {
					skew := serverTime.Sub(time.Now()).Seconds()
					adjustClockSkew(skew)
				}
			}
		}
		return nil, fmt.Errorf("edge TTS dial failed: %w", err)
	}
	defer conn.Close()

	// 1. Send speech.config
	configMsg := "Content-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
		return nil, fmt.Errorf("send speech.config failed: %w", err)
	}

	// 2. Send SSML
	reqID := genRandHex(16)
	escapedText := xmlEscape(text)
	ssml := fmt.Sprintf("X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nPath:ssml\r\n\r\n"+
		"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='ru-RU'>"+
		"<voice name='%s'><prosody pitch='+0Hz' rate='+0%%' volume='+0%%'>%s</prosody></voice></speak>",
		reqID, voice, escapedText)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(ssml)); err != nil {
		return nil, fmt.Errorf("send ssml failed: %w", err)
	}

	// 3. Receive audio chunks
	var mp3Buf bytes.Buffer
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				break
			}
			return nil, fmt.Errorf("read edge tts message failed: %w", err)
		}

		if msgType == websocket.TextMessage {
			if strings.Contains(string(payload), "Path:turn.end") {
				break
			}
		} else if msgType == websocket.BinaryMessage && len(payload) > 2 {
			headerLen := binary.BigEndian.Uint16(payload[:2])
			if len(payload) > int(headerLen)+2 {
				mp3Buf.Write(payload[2+headerLen:])
			}
		}
	}

	if mp3Buf.Len() == 0 {
		return nil, errors.New("edge TTS returned zero audio data")
	}

	// 4. Decode MP3
	dec, err := mp3.NewDecoder(bytes.NewReader(mp3Buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("mp3 decode error: %w", err)
	}

	srcSampleRate := dec.SampleRate() // 24000
	monoPCM, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("mp3 pcm read error: %w", err)
	}

	// 5. Resample to 48000 Hz, stereo, 16-bit
	return resampleTo48kStereo(monoPCM, srcSampleRate), nil
}

func resampleTo48kStereo(monoPCM []byte, inRate int) []byte {
	if inRate <= 0 {
		inRate = 24000
	}
	repeatRatio := 48000 / inRate
	if repeatRatio < 1 {
		repeatRatio = 1
	}

	numSamples := len(monoPCM) / 2
	out := make([]byte, numSamples*repeatRatio*4) // 4 bytes per stereo sample (2 bytes L, 2 bytes R)
	outIdx := 0

	for i := 0; i < numSamples; i++ {
		sampleBytes := monoPCM[i*2 : i*2+2]
		for r := 0; r < repeatRatio; r++ {
			// Left channel
			out[outIdx] = sampleBytes[0]
			out[outIdx+1] = sampleBytes[1]
			// Right channel
			out[outIdx+2] = sampleBytes[0]
			out[outIdx+3] = sampleBytes[1]
			outIdx += 4
		}
	}
	return out[:outIdx]
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
