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

func TestParseHangupCommand(t *testing.T) {
	tests := []struct {
		input      string
		wantHangup bool
	}{
		// Should NOT hang up (false positive risks)
		{"покажи погоду в Москве", false},
		{"покажи курс доллара", false},
		{"покажи последние новости", false},
		{"пока что всё нормально", false},
		{"пока не знаю", false},
		{"пока ты ищешь, я схожу за чаем", false},
		{"хватит ли мне 100 рублей?", false},
		{"автостопом по галактике", false},
		{"покажешь фокус?", false},

		// SHOULD hang up (true farewells)
		{"пока", true},
		{"пока-пока", true},
		{"ну пока", true},
		{"ладно пока", true},
		{"ну всё, пока!", true},
		{"до свидания", true},
		{"до встречи", true},
		{"до связи", true},
		{"стоп", true},
		{"хватит", true},
		{"отбой", true},
		{"заверши сессию", true},
		{"завершить сессию", true},
		{"заверши разговор", true},
		{"завершить звонок", true},
		{"отключись", true},
		{"выключись", true},
		{"конец связи", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, _ := parseHangupCommand(tc.input)
			if got != tc.wantHangup {
				t.Errorf("parseHangupCommand(%q) = %v; want %v", tc.input, got, tc.wantHangup)
			}
		})
	}
}

func TestCleanSearchQuery(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			"Расскажи мне , посмотри в интернете, в Википедии историю появления породы собаки русский спаниель и расскажи .",
			"историю появления породы собаки русский спаниель",
		},
		{
			"посмотри в интернете кто президент Франции",
			"кто президент франции",
		},
		{
			"найди в википедии квантовый процессор",
			"квантовый процессор",
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := cleanSearchQuery(tc.input)
			if got != tc.want {
				t.Errorf("cleanSearchQuery(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}
