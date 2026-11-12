package voice

import (
	"testing"
)

func TestParseVolumeCommand(t *testing.T) {
	tests := []struct {
		input     string
		wantIsVol bool
		wantVol   int
		wantDelta int
	}{
		{"сделай потише", true, -1, -15},
		{"тише пожалуйста", true, -1, -15},
		{"погромче сделай", true, -1, 15},
		{"прибавь звук", true, -1, 15},
		{"громкость 50%", true, 50, 0},
		{"поставь громкость 80", true, 80, 0},
		{"громкость 7", true, 70, 0}, // 1-10 scale
		{"громкость на пять", true, 50, 0},
		{"звук на максимум", true, 100, 0},
		{"звук на минимум", true, 10, 0},
		{"выключи звук", true, 0, 0},
		{"без звука", true, 0, 0},
		{"какая сегодня погода", false, -1, 0},
		{"расскажи анекдот", false, -1, 0},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			isVol, vol, delta := parseVolumeCommand(tc.input)
			if isVol != tc.wantIsVol || vol != tc.wantVol || delta != tc.wantDelta {
				t.Errorf("parseVolumeCommand(%q) = (%v, %d, %d); want (%v, %d, %d)",
					tc.input, isVol, vol, delta, tc.wantIsVol, tc.wantVol, tc.wantDelta)
			}
		})
	}
}

func TestDetectSearchQuery(t *testing.T) {
	tests := []struct {
		input     string
		wantFound bool
	}{
		{"какой сейчас курс доллара", true},
		{"сколько стоит биткоин", true},
		{"какая погода в Москве", true},
		{"свежие новости за сегодня", true},
		{"найди в интернете рецепт пиццы", true},
		{"посмотри в duckduckgo события 13 ноября", true},
		{"кто такой Илон Маск", true},
		{"что такое квантовый компьютер", true},
		{"привет, как твои дела?", false},
		{"сделай погромче", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			found, query := detectSearchQuery(tc.input)
			if found != tc.wantFound {
				t.Errorf("detectSearchQuery(%q) found=%v, query=%q; want found=%v",
					tc.input, found, query, tc.wantFound)
			}
		})
	}
}

func TestCleanHTML(t *testing.T) {
	input := `<b>Курс</b> <i>доллара</i> &mdash; 81.35 &amp; руб.`
	got := cleanHTML(input)
	want := "Курс доллара — 81.35 & руб."
	if got != want {
		t.Errorf("cleanHTML(%q) = %q; want %q", input, got, want)
	}
}
