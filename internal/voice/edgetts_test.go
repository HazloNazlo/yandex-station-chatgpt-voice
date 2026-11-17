package voice

import (
	"context"
	"testing"
	"time"
)

func TestSynthesizeEdgeTTS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pcm, err := synthesizeEdgeTTS(ctx, "Проверка связи. Погода в Москве отличная.", "ru-RU-SvetlanaNeural")
	if err != nil {
		t.Fatalf("synthesizeEdgeTTS failed: %v", err)
	}

	if len(pcm) == 0 {
		t.Fatal("synthesizeEdgeTTS returned empty PCM")
	}

	// 48000 Hz, stereo 16-bit = 192000 bytes/sec
	// 2-3 seconds of speech should be at least 150000 bytes
	t.Logf("synthesizeEdgeTTS returned %d bytes of 48k stereo PCM", len(pcm))
	if len(pcm) < 50000 {
		t.Errorf("PCM too short: got %d bytes", len(pcm))
	}
}
