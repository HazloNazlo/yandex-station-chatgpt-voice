package voice

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
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
		{"кто президент Франции", true},
		{"кто написал Войну и мир", true},
		{"что такое квантовый компьютер", true},
		{"где находится Байкал", true},
		{"когда родился Пушкин", true},
		{"расскажи историю породы русский спаниель", true},
		{"объясни как работает блокчейн", true},
		{"у тебя есть интернет", true},
		{"проверь интернет", true},
		{"привет", false},
		{"привет, как твои дела?", false},
		{"как дела", false},
		{"спасибо", false},
		{"ясно", false},
		{"понял", false},
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
		{"пока я на работе", false},
		{"пока ещё рано", false},
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
		{"замолчи", true},
		{"заверши сессию", true},
		{"завершить сессию", true},
		{"заверши сеанс", true},
		{"завершить сеанс", true},
		{"завершай сеанс", true},
		{"закончи сеанс", true},
		{"закрой сеанс", true},
		{"Завер... Заверши сеанс", true},
		{"давай завершим", true},
		{"закончи диалог", true},
		{"заверши сеанс пожалуйста", true},
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

func TestSearchWebLive(t *testing.T) {
	if os.Getenv("VOICE_LIVE_TOOLS_TESTS") != "1" {
		t.Skip("set VOICE_LIVE_TOOLS_TESTS=1 for external provider integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	queries := []string{
		"какой курс доллара",
		"какая погода в Калуге",
		"сколько стоит биткоин",
		"у тебя есть интернет",
		"какое сегодня число",
	}

	for _, q := range queries {
		res, err := searchWeb(ctx, q)
		if err != nil {
			t.Errorf("searchWeb(%q) error = %v", q, err)
		}
		t.Logf("Query: %q -> Result: %q", q, res)
		if res == "" {
			t.Errorf("searchWeb(%q) returned empty result", q)
		}
	}
}

func TestOpenURL_SSRF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	blockedURLs := []string{
		"http://127.0.0.1:8080/test",
		"http://localhost:8080/secret",
		"http://192.168.1.1/admin",
		"http://10.0.0.1/status",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
		"ftp://example.com/file",
	}

	for _, u := range blockedURLs {
		res, err := openURL(ctx, u)
		if err == nil {
			t.Errorf("openURL(%q) should have been blocked for SSRF/scheme, got success: %q", u, res)
		} else {
			t.Logf("openURL(%q) properly blocked: %v", u, err)
		}
	}
}

func TestOpenURL_Public(t *testing.T) {
	if os.Getenv("VOICE_LIVE_TOOLS_TESTS") != "1" {
		t.Skip("set VOICE_LIVE_TOOLS_TESTS=1 for external provider integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := openURL(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("openURL(https://example.com) failed: %v", err)
	}
	t.Logf("openURL(https://example.com) -> %s", res)
	if !strings.Contains(res, "example.com") || !strings.Contains(res, "Example Domain") {
		t.Errorf("unexpected content from example.com: %s", res)
	}
}

func TestTavilySearch(t *testing.T) {
	if os.Getenv("VOICE_LIVE_TOOLS_TESTS") != "1" {
		t.Skip("set VOICE_LIVE_TOOLS_TESTS=1 for external provider integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := fetchTavilySearch(ctx, "Кто президент Франции?")
	if err != nil {
		t.Fatalf("fetchTavilySearch failed: %v", err)
	}
	t.Logf("Tavily search answer: %s", res)
	if !strings.Contains(strings.ToLower(res), "макрон") {
		t.Errorf("expected mention of Macron, got: %s", res)
	}
}

func TestWeather(t *testing.T) {
	if os.Getenv("VOICE_LIVE_TOOLS_TESTS") != "1" {
		t.Skip("set VOICE_LIVE_TOOLS_TESTS=1 for external provider integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	testQueries := []struct {
		query    string
		wantWord string
	}{
		{"погода в Калуге", "Калуг"},
		{"какая погода в Калуге сейчас", "Калуг"},
		{"погода в Москве на завтра", "Москв"},
		{"какая сейчас погода", "Москв"},
	}

	for _, tc := range testQueries {
		res, err := fetchWeather(ctx, tc.query)
		if err != nil {
			t.Fatalf("fetchWeather(%q) failed: %v", tc.query, err)
		}
		t.Logf("Weather %q: %s", tc.query, res)
		if !strings.Contains(res, tc.wantWord) {
			t.Errorf("expected %q in weather for %q, got: %s", tc.wantWord, tc.query, res)
		}
	}
}

func TestDetectURLQuery(t *testing.T) {
	tests := []struct {
		input     string
		wantFound bool
	}{
		{"открой example.org и расскажи что там", true},
		{"посмотри сайт lenta.ru", true},
		{"что на сайте rbc.ru", true},
		{"зайди на https://habr.com", true},
		{"прочитай https://golang.org/doc", true},
		{"привет как дела", false},
		{"какая погода в Москве", false},
	}

	for _, tc := range tests {
		gotFound, gotURL := detectURLQuery(tc.input)
		if gotFound != tc.wantFound {
			t.Errorf("detectURLQuery(%q) found=%v, url=%q; want %v", tc.input, gotFound, gotURL, tc.wantFound)
		}
	}
}
