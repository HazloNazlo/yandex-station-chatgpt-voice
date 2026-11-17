package voice

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func mockToolHTTP(t *testing.T, respond func(*http.Request) (int, string)) {
	t.Helper()
	previous := sharedHTTPClient
	sharedHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := respond(req)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	cbrMu.Lock()
	previousCBR, previousAt := cbrCached, cbrCachedAt
	cbrCached, cbrCachedAt = "", time.Time{}
	cbrMu.Unlock()
	weatherMu.Lock()
	previousWeather, previousWeatherAt := weatherCached, weatherCachedAt
	weatherCached, weatherCachedAt = make(map[string]string), make(map[string]time.Time)
	weatherMu.Unlock()
	t.Cleanup(func() {
		sharedHTTPClient = previous
		cbrMu.Lock()
		cbrCached, cbrCachedAt = previousCBR, previousAt
		cbrMu.Unlock()
		weatherMu.Lock()
		weatherCached, weatherCachedAt = previousWeather, previousWeatherAt
		weatherMu.Unlock()
	})
}

func TestCBRValidatesDateNominalAndRoundingCarry(t *testing.T) {
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Hostname() != "www.cbr-xml-daily.ru" {
			t.Error("wrong rates provider", req.URL.Hostname())
		}
		return 200, `{"Date":"2026-11-17T11:30:00+03:00","Valute":{"USD":{"Nominal":1,"Value":82.995},"EUR":{"Nominal":1,"Value":90.01},"CNY":{"Nominal":10,"Value":112.0}}}`
	})
	answer, err := fetchCBRRates(t.Context())
	if err != nil || !strings.Contains(answer, "17.11.2026") || !strings.Contains(answer, "доллар 83 рубля 0 копеек") ||
		!strings.Contains(answer, "юань 11 рублей 20 копеек") || !strings.Contains(answer, "не курс покупки") {
		t.Fatal("invalid nominal/date/cent-carry semantics", answer, err)
	}
}

func TestCurrencyFailureNeverFallsThroughToEncyclopedia(t *testing.T) {
	calls := 0
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		calls++
		if req.URL.Hostname() != "www.cbr-xml-daily.ru" {
			t.Error("unavailable current data fell through", req.URL.Hostname())
		}
		return 429, `{"error":"rate limit"}`
	})
	answer, err := searchWeb(t.Context(), "какой сейчас курс доллара")
	if err == nil || answer != "" || calls != 1 {
		t.Fatal("provider outage was hidden", calls, answer, err)
	}
}

func TestCBRRejectsIncompleteAndErrorObjects(t *testing.T) {
	for _, body := range []string{`{}`, `{"error":"unavailable"}`, `{"Date":"invalid","Valute":{}}`,
		`{"Date":"2026-11-17T11:30:00+03:00","Valute":{"USD":{"Nominal":0,"Value":82.0}}}`} {
		t.Run(body, func(t *testing.T) {
			mockToolHTTP(t, func(*http.Request) (int, string) { return 200, body })
			if answer, err := fetchCBRRates(t.Context()); err == nil || answer != "" {
				t.Fatal("incomplete rate became authoritative", answer, err)
			}
		})
	}
}

func TestCryptoQuoteKeepsExchangeSymbolAndUSDTUnits(t *testing.T) {
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Query().Get("symbol") != "ETHUSDT" {
			t.Error("wrong exchange pair")
		}
		return 200, `{"symbol":"ETHUSDT","price":"2345.67"}`
	})
	answer, err := fetchCryptoPrice(t.Context(), "ETH")
	if err != nil || !strings.Contains(answer, "эфир") || !strings.Contains(answer, "2345.67 USDT") || !strings.Contains(answer, "Binance") {
		t.Fatal("quote lost asset/exchange/units", answer, err)
	}
}

func TestCryptoRejectsWrongPairAndEmptyPrices(t *testing.T) {
	for _, body := range []string{`{"symbol":"BTCUSDT","price":""}`, `{"symbol":"ETHUSDT","price":"100"}`, `{"symbol":"BTCUSDT","price":"NaN"}`, `{"symbol":"BTCUSDT","price":"-1"}`} {
		t.Run(body, func(t *testing.T) {
			mockToolHTTP(t, func(*http.Request) (int, string) { return 200, body })
			if answer, err := fetchCryptoPrice(t.Context(), "BTC"); err == nil || answer != "" {
				t.Fatal("invalid crypto quote became speech", answer, err)
			}
		})
	}
}

const geoFixture = `{"results":[{"name":"Калуга","latitude":54.51,"longitude":36.26,"country":"Россия"}]}`

func TestWeatherRejectsMissingGeocodingCoordinates(t *testing.T) {
	calls := 0
	mockToolHTTP(t, func(*http.Request) (int, string) {
		calls++
		return 200, `{"results":[{"name":"Калуга","country":"Россия"}]}`
	})
	if answer, err := fetchOpenMeteoWeather(t.Context(), "Калуга", "погода сейчас"); err == nil || answer != "" || calls != 1 {
		t.Fatal("missing coordinates became weather for zero latitude/longitude", answer, err, calls)
	}
}

func TestWeatherCacheEvictsOldestEntryAtBound(t *testing.T) {
	mockToolHTTP(t, func(*http.Request) (int, string) { return 503, "" })
	for i := range 64 {
		cacheWeather(string(rune('A'+i)), "dated weather")
	}
	weatherMu.Lock()
	weatherCachedAt["A"] = time.Unix(1, 0)
	weatherMu.Unlock()
	cacheWeather("newest", "new weather")
	weatherMu.Lock()
	defer weatherMu.Unlock()
	if len(weatherCached) != 64 || weatherCached["A"] != "" || weatherCached["newest"] != "new weather" {
		t.Fatal("weather cache did not enforce its size/oldest eviction contract")
	}
}

func TestWeatherMissingDataDoesNotBecomeZeroDegrees(t *testing.T) {
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Hostname() == "geocoding-api.open-meteo.com" {
			return 200, geoFixture
		}
		return 200, `{"current":{"time":"2026-11-17T13:00","weather_code":0,"wind_speed_10m":2}}`
	})
	if answer, err := fetchOpenMeteoWeather(t.Context(), "Калуга", "погода сейчас"); err == nil || answer != "" {
		t.Fatal("missing temperature became zero", answer, err)
	}
}

func TestWeatherZeroIsValidAndSourceTimeIsSpoken(t *testing.T) {
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Hostname() == "geocoding-api.open-meteo.com" {
			return 200, geoFixture
		}
		return 200, `{"current":{"time":"2026-11-17T13:00","temperature_2m":0,"weather_code":0,"wind_speed_10m":2}}`
	})
	answer, err := fetchOpenMeteoWeather(t.Context(), "Калуга", "погода сейчас")
	if err != nil || !strings.Contains(answer, "ноль градусов") || !strings.Contains(answer, "17.11.2026") || !strings.Contains(answer, "13:00") || !strings.Contains(answer, "Open-Meteo") {
		t.Fatal("valid zero/time/source was lost", answer, err)
	}
}

func TestForecastArrayMismatchFailsWithoutCurrentWeatherSubstitution(t *testing.T) {
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Hostname() == "geocoding-api.open-meteo.com" {
			return 200, geoFixture
		}
		return 200, `{"daily":{"time":["2026-11-17","2026-11-18"],"weather_code":[0,3],"temperature_2m_max":[2],"temperature_2m_min":[0]}}`
	})
	if answer, err := fetchOpenMeteoWeather(t.Context(), "Калуга", "погода завтра"); err == nil || answer != "" {
		t.Fatal("short daily arrays became a forecast", answer, err)
	}
}

func TestFutureWeatherDoesNotFallBackToCurrentWttr(t *testing.T) {
	calls := 0
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		calls++
		if req.URL.Hostname() == "wttr.in" {
			t.Error("future forecast fell back to current conditions")
		}
		return 503, `{}`
	})
	if answer, err := fetchWeather(t.Context(), "погода в Калуге завтра"); err == nil || answer != "" || calls != 1 {
		t.Fatal("forecast outage was hidden", answer, err, calls)
	}
}

func TestFreshNewsDoesNotFallBackToWikipedia(t *testing.T) {
	calls := 0
	mockToolHTTP(t, func(req *http.Request) (int, string) {
		calls++
		if req.URL.Hostname() != "html.duckduckgo.com" {
			t.Error("current news used encyclopedia", req.URL.Hostname())
		}
		return 503, "unavailable"
	})
	if answer, err := searchWeb(context.Background(), "свежие новости за сегодня"); err == nil || answer != "" || calls != 1 {
		t.Fatal("news outage was hidden", answer, err, calls)
	}
}

func TestToolJSONBoundsAndStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{503, `{}`}, {200, strings.Repeat(" ", 33)}, {200, `{"x":1} trailing`}} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		var target map[string]any
		if err := decodeToolJSON(resp, 32, &target); err == nil {
			t.Fatal("invalid bounded JSON accepted", tc.status)
		}
	}
}
