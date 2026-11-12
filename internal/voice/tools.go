package voice

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	volumeMu     sync.Mutex
	cachedVolume = -1

	snippetRegex = regexp.MustCompile(`<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	titleRegex   = regexp.MustCompile(`<a[^>]+class="result__a"[^>]*>(.*?)</a>`)
	tagRegex     = regexp.MustCompile(`<[^>]+>`)
	wsRegex      = regexp.MustCompile(`\s+`)
)

func cleanHTML(s string) string {
	s = tagRegex.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = wsRegex.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func readCurrentVolume() int {
	volumeMu.Lock()
	defer volumeMu.Unlock()

	if data, err := os.ReadFile("/bkupgrade/data/quasar/last_user_volume"); err == nil {
		s := strings.TrimSpace(string(data))
		if val, err := strconv.ParseFloat(s, 64); err == nil && val >= 0 && val <= 10 {
			cachedVolume = int(val * 10)
			return cachedVolume
		}
	}

	cmd := exec.Command("/system/workdir/bin/amixer", "sget", "Master")
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/system/vendor/quasar/libs")
	if out, err := cmd.Output(); err == nil {
		re := regexp.MustCompile(`\[(\d+)%\]`)
		if m := re.FindSubmatch(out); len(m) > 1 {
			if pct, err := strconv.Atoi(string(m[1])); err == nil {
				cachedVolume = pct
				return cachedVolume
			}
		}
	}

	if cachedVolume >= 0 {
		return cachedVolume
	}
	return 50
}

func setStationVolume(percent int) (int, error) {
	volumeMu.Lock()
	defer volumeMu.Unlock()

	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}

	cmd := exec.Command("/system/vendor/quasar/set_volume.sh", strconv.Itoa(percent))
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/system/vendor/quasar/libs")
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("set_volume.sh failed: %w", err)
	}

	cachedVolume = percent
	valStr := fmt.Sprintf("%.6f\n", float64(percent)/10.0)
	_ = os.WriteFile("/bkupgrade/data/quasar/last_user_volume", []byte(valStr), 0644)

	return percent, nil
}

func adjustStationVolume(delta int) (int, error) {
	cur := readCurrentVolume()
	return setStationVolume(cur + delta)
}

func parseVolumeCommand(text string) (bool, int, int) {
	lower := strings.ToLower(text)

	// Direct presets
	if strings.Contains(lower, "на максимум") || strings.Contains(lower, "максимальная громкость") {
		return true, 100, 0
	}
	if strings.Contains(lower, "на минимум") || strings.Contains(lower, "минимальная громкость") {
		return true, 10, 0
	}
	if strings.Contains(lower, "выключи звук") || strings.Contains(lower, "без звука") || strings.Contains(lower, "отключи звук") {
		return true, 0, 0
	}

	// Named numbers 1..10 in Russian: "громкость на пять" / "громкость пять"
	namedNums := map[string]int{
		"ноль": 0, "один": 10, "два": 20, "три": 30, "четыре": 40,
		"пять": 50, "шесть": 60, "семь": 70, "восемь": 80, "девять": 90, "десять": 100,
	}
	for name, val := range namedNums {
		if strings.Contains(lower, "громкост") && (strings.Contains(lower, " на "+name) || strings.Contains(lower, " "+name)) {
			return true, val, 0
		}
	}

	// Check numeric: "громкость 60", "громкость на 70%", "поставь громкость 5", "звук 4"
	reAbs := regexp.MustCompile(`(?:громкост[ьи]|звук)[^\d]{0,10}(\d{1,3})%?`)
	if m := reAbs.FindStringSubmatch(lower); len(m) > 1 {
		if val, err := strconv.Atoi(m[1]); err == nil && val >= 0 && val <= 100 {
			if val <= 10 && val > 0 {
				val *= 10 // Convert 1-10 station scale to 10-100%
			}
			return true, val, 0
		}
	}

	// Check relative quieter: "потише", "тише", "убавь", "сделай тише", "уменьши громкость"
	reQuieter := regexp.MustCompile(`(потише|тише|убавь|сделай тише|уменьши громкость|уменьши звук|сделай звук тише|приглуши)`)
	if reQuieter.MatchString(lower) {
		return true, -1, -15
	}

	// Check relative louder: "погромче", "громче", "прибавь", "сделай громче", "увеличь громкость"
	reLouder := regexp.MustCompile(`(погромче|громче|прибавь|сделай громче|увеличь громкость|увеличь звук|сделай звук громче)`)
	if reLouder.MatchString(lower) {
		return true, -1, 15
	}

	return false, -1, 0
}

func detectSearchQuery(text string) (bool, string) {
	lower := strings.ToLower(text)

	// Direct mentions of internet / search engines
	if strings.Contains(lower, "интернет") || strings.Contains(lower, "duckduckgo") ||
		strings.Contains(lower, "дакдакго") || strings.Contains(lower, "дактдакго") ||
		strings.Contains(lower, "в гугле") || strings.Contains(lower, "в яндексе") ||
		strings.Contains(lower, "погугли") || strings.Contains(lower, "загугли") {
		return true, strings.TrimSpace(text)
	}

	// Explicit search triggers
	reExplicit := regexp.MustCompile(`(?:найди|поищи|посмотри|глянь|пробей|узнай|проверь|досмотри|поиск)\s*(?:в интернете|в сети|в новостях)?\s*(.*)`)
	if m := reExplicit.FindStringSubmatch(lower); len(m) > 1 {
		q := strings.TrimSpace(m[1])
		if len(q) > 3 {
			return true, q
		}
	}

	// Keywords requiring internet / fresh data
	triggers := []string{
		"курс", "доллар", "евро", "юан", "биткоин", "bitcoin", "btc", "крипт", "валют", "акци", "цена",
		"погод", "температур", "прогноз", "градус", "дождь", "снег",
		"новост", "событи", "произошл", "случилос", "что нового", "дайджест", "сводка",
		"число", "день недели", "который час", "сколько времени", "какая дата",
		"кто такой", "что такое", "где находится", "расскажи про", "биография",
	}

	for _, trig := range triggers {
		if strings.Contains(lower, trig) {
			return true, strings.TrimSpace(text)
		}
	}

	return false, ""
}

type volumeArgs struct {
	Volume    *int   `json:"volume,omitempty"`
	Delta     *int   `json:"delta,omitempty"`
	Direction string `json:"direction,omitempty"`
}

func handleSetVolume(argsJSON string) (string, error) {
	var args volumeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		trimmed := strings.TrimSpace(argsJSON)
		if v, err := strconv.Atoi(trimmed); err == nil {
			newVol, err := setStationVolume(v)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`{"status":"ok","volume":%d,"message":"Громкость установлена на %d%%"}`, newVol, newVol), nil
		}
	}

	if args.Volume != nil {
		newVol, err := setStationVolume(*args.Volume)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"ok","volume":%d,"message":"Громкость установлена на %d%%"}`, newVol, newVol), nil
	}

	delta := 0
	if args.Delta != nil {
		delta = *args.Delta
	} else if strings.EqualFold(args.Direction, "up") || strings.EqualFold(args.Direction, "louder") {
		delta = 15
	} else if strings.EqualFold(args.Direction, "down") || strings.EqualFold(args.Direction, "quieter") {
		delta = -15
	} else {
		cur := readCurrentVolume()
		return fmt.Sprintf(`{"status":"ok","volume":%d,"message":"Текущая громкость %d%%"}`, cur, cur), nil
	}

	newVol, err := adjustStationVolume(delta)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"status":"ok","volume":%d,"message":"Громкость изменена на %d%%"}`, newVol, newVol), nil
}

func fetchCBRRates(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cbr-xml-daily.ru/daily_json.js", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data struct {
		Date   string `json:"Date"`
		Valute map[string]struct {
			CharCode string  `json:"CharCode"`
			Nominal  int     `json:"Nominal"`
			Name     string  `json:"Name"`
			Value    float64 `json:"Value"`
			Previous float64 `json:"Previous"`
		} `json:"Valute"`
	}
	if err := json.UnmarshalRead(resp.Body, &data); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("Официальные курсы валют ЦБ РФ:\n")
	for _, code := range []string{"USD", "EUR", "CNY"} {
		if v, ok := data.Valute[code]; ok {
			diff := v.Value - v.Previous
			diffStr := ""
			if diff > 0 {
				diffStr = fmt.Sprintf(" (+%.2f)", diff)
			} else if diff < 0 {
				diffStr = fmt.Sprintf(" (%.2f)", diff)
			}
			sb.WriteString(fmt.Sprintf("- %s (%s): %.2f ₽%s\n", v.Name, v.CharCode, v.Value, diffStr))
		}
	}
	return sb.String(), nil
}

func fetchCryptoPrice(ctx context.Context, symbol string) (string, error) {
	reqURL := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%sUSDT", strings.ToUpper(symbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := json.UnmarshalRead(resp.Body, &data); err != nil {
		return "", err
	}
	if p, err := strconv.ParseFloat(data.Price, 64); err == nil {
		return fmt.Sprintf("Курс %s: $%.2f USD (на бирже Binance).", symbol, p), nil
	}
	return fmt.Sprintf("Курс %s: %s USD.", symbol, data.Price), nil
}

func fetchWeather(ctx context.Context, query string) (string, error) {
	city := ""
	lower := strings.ToLower(query)
	reCity := regexp.MustCompile(`(?:в|во|для|город[ае]?)\s+([а-яёa-z-]+)`)
	if m := reCity.FindStringSubmatch(lower); len(m) > 1 {
		city = m[1]
	}

	reqURL := "https://wttr.in/" + url.PathEscape(city) + "?m&M&format=4&lang=ru"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/7.88.1")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(string(body))
	if res == "" || strings.Contains(res, "Unknown location") {
		return "", errors.New("location not found")
	}
	return fmt.Sprintf("Погода сейчас: %s", res), nil
}

func fetchWikipedia(ctx context.Context, query string) (string, error) {
	reqURL := "https://ru.wikipedia.org/w/api.php?action=query&list=search&srsearch=" + url.QueryEscape(query) + "&utf8=&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.UnmarshalRead(resp.Body, &data); err != nil {
		return "", err
	}

	if len(data.Query.Search) == 0 {
		return "", errors.New("no wikipedia results")
	}

	var sb strings.Builder
	sb.WriteString("Данные из энциклопедии:\n")
	limit := 3
	if len(data.Query.Search) < limit {
		limit = len(data.Query.Search)
	}
	for i := 0; i < limit; i++ {
		item := data.Query.Search[i]
		cleanSnip := cleanHTML(item.Snippet)
		sb.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, item.Title, cleanSnip))
	}
	return sb.String(), nil
}

func fetchDuckDuckGo(ctx context.Context, query string) (string, error) {
	postData := url.Values{"q": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://html.duckduckgo.com/html/", strings.NewReader(postData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")

	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search HTTP status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("reading search response failed: %w", err)
	}

	body := string(bodyBytes)
	snippets := snippetRegex.FindAllStringSubmatch(body, 4)
	titles := titleRegex.FindAllStringSubmatch(body, 4)

	if len(snippets) == 0 {
		return "", errors.New("no snippets found")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Результаты поиска в интернете по запросу %q:\n", query))
	for i := range snippets {
		title := ""
		if i < len(titles) && len(titles[i]) > 1 {
			title = cleanHTML(titles[i][1])
		}
		snippet := ""
		if len(snippets[i]) > 1 {
			snippet = cleanHTML(snippets[i][1])
		}
		if title != "" {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, title))
		}
		if snippet != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", snippet))
		}
	}
	return sb.String(), nil
}

func searchWeb(ctx context.Context, query string) (string, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return `{"error":"empty query"}`, nil
	}
	lower := strings.ToLower(trimmed)

	// 0. Capability inquiry: does ChatGPT have internet / DuckDuckGo access?
	if (strings.Contains(lower, "доступ") && strings.Contains(lower, "интернет")) ||
		(strings.Contains(lower, "duckduckgo") && (strings.Contains(lower, "доступ") || strings.Contains(lower, "можешь") || strings.Contains(lower, "умеешь") || strings.Contains(lower, "есть"))) {
		return "Да, у тебя есть полноценный доступ в интернет! Твоя Яндекс Станция подключена к поисковику DuckDuckGo, курсам валют ЦБ РФ, криптобиржам и прогнозу погоды. Ты можешь искать любую актуальную информацию в сети в реальном времени.", nil
	}

	// 1. Time / Date query
	if strings.Contains(lower, "число") || strings.Contains(lower, "день недели") ||
		strings.Contains(lower, "который час") || strings.Contains(lower, "сколько времени") ||
		strings.Contains(lower, "какой сегодня день") || strings.Contains(lower, "какая дата") {
		loc := time.FixedZone("MSK", 3*3600)
		now := time.Now().In(loc)
		months := []string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
		weekdays := []string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
		msg := fmt.Sprintf("Текущее время и дата: %02d:%02d, %s, %d %s %d года (московское время).",
			now.Hour(), now.Minute(), weekdays[now.Weekday()], now.Day(), months[now.Month()-1], now.Year())
		return msg, nil
	}

	// 2. Crypto query
	if strings.Contains(lower, "биткоин") || strings.Contains(lower, "bitcoin") || strings.Contains(lower, "btc") {
		if res, err := fetchCryptoPrice(ctx, "BTC"); err == nil {
			return res, nil
		}
	}

	// 3. Fiat currency query
	if strings.Contains(lower, "доллар") || strings.Contains(lower, "евро") ||
		strings.Contains(lower, "юан") || strings.Contains(lower, "курс валют") ||
		strings.Contains(lower, "курс рубля") || strings.Contains(lower, "какой курс") {
		if res, err := fetchCBRRates(ctx); err == nil {
			return res, nil
		}
	}

	// 4. Weather query
	if strings.Contains(lower, "погод") || strings.Contains(lower, "температур") ||
		strings.Contains(lower, "градус") || strings.Contains(lower, "дождь") || strings.Contains(lower, "снег") {
		if res, err := fetchWeather(ctx, trimmed); err == nil {
			return res, nil
		}
	}

	// 5. General search via DuckDuckGo POST
	if res, err := fetchDuckDuckGo(ctx, trimmed); err == nil && res != "" {
		return res, nil
	}

	// 6. Fallback to Wikipedia
	if res, err := fetchWikipedia(ctx, trimmed); err == nil && res != "" {
		return res, nil
	}

	return fmt.Sprintf("По запросу %q актуальной информации не найдено.", trimmed), nil
}

type searchArgs struct {
	Query string `json:"query"`
}

func handleSearchWeb(ctx context.Context, argsJSON string) (string, error) {
	var args searchArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Query == "" {
		args.Query = strings.Trim(strings.TrimSpace(argsJSON), `"{}`)
	}
	return searchWeb(ctx, args.Query)
}

func getSessionUpdateMessage() string {
	tools := []map[string]any{
		{
			"type": "function",
			"name": "set_volume",
			"description": "Управление громкостью колонки Яндекс Станция (от 0 до 100%). Используй, когда пользователь просит изменить громкость, сделать тише или громче.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"volume": map[string]any{
						"type":        "integer",
						"description": "Абсолютный уровень громкости от 0 до 100",
					},
					"delta": map[string]any{
						"type":        "integer",
						"description": "Изменение громкости относительно текущей (например, 15 для громче, -15 для тише)",
					},
					"direction": map[string]any{
						"type":        "string",
						"enum":        []string{"up", "down"},
						"description": "'up' (громче) или 'down' (тише)",
					},
				},
			},
		},
		{
			"type": "function",
			"name": "search_web",
			"description": "Поиск актуальной информации в интернете: свежие новости, курс валют и акций, погода, факты и события сегодняшнего дня.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Поисковый запрос на русском или английском",
					},
				},
				"required": []string{"query"},
			},
		},
	}

	payload := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": "Ты умный голосовой помощник на Яндекс Станции Мини. Тебя зовут ChatGPT. Ты общаешься голосом через динамик колонки. Отвечай кратко, емко и по существу (1-2 предложения), без лишних вступительных фраз. Если пользователь просит найти свежие новости, актуальный курс доллара/евро, погоду или информацию из интернета, обязательно вызывай search_web. Если пользователь просит сделать громче, тише или задать громкость, обязательно вызывай set_volume.",
			"tools":        tools,
			"tool_choice":  "auto",
		},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func getSystemInstructionMessage() string {
	payload := map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "system",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": "Ты голосовой помощник на Яндекс Станции Мини. Отвечай кратко, 1-2 предложениями. Для актуальной информации в интернете (погода, новости, курсы валют) вызывай функцию search_web. Для управления громкостью вызывай функцию set_volume.",
				},
			},
		},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

type toolCallInfo struct {
	CallID    string
	Name      string
	Arguments string
}

func extractWirePayload(data []byte) ([]byte, string) {
	var raw struct {
		Type    string         `json:"type"`
		Data    jsontext.Value `json:"data"`
		Payload jsontext.Value `json:"payload"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return data, ""
	}
	if raw.Type == "data_message" && len(raw.Data) > 0 {
		var inner string
		if json.Unmarshal(raw.Data, &inner) == nil {
			return extractWirePayload([]byte(inner))
		}
	}
	if len(raw.Payload) > 0 {
		var nested struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw.Payload, &nested) == nil && nested.Type != "" {
			return raw.Payload, nested.Type
		}
	}
	return data, raw.Type
}

func parseToolCalls(data []byte) []toolCallInfo {
	unwrapped, eventType := extractWirePayload(data)
	var calls []toolCallInfo

	switch eventType {
	case "response.function_call_arguments.done":
		var ev struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(unwrapped, &ev) == nil && ev.CallID != "" && ev.Name != "" {
			calls = append(calls, toolCallInfo{
				CallID:    ev.CallID,
				Name:      ev.Name,
				Arguments: ev.Arguments,
			})
		}
	case "response.output_item.done":
		var ev struct {
			Item struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
		}
		if json.Unmarshal(unwrapped, &ev) == nil && ev.Item.Type == "function_call" && ev.Item.CallID != "" {
			calls = append(calls, toolCallInfo{
				CallID:    ev.Item.CallID,
				Name:      ev.Item.Name,
				Arguments: ev.Item.Arguments,
			})
		}
	case "response.done":
		var ev struct {
			Response struct {
				Output []struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(unwrapped, &ev) == nil {
			for _, item := range ev.Response.Output {
				if item.Type == "function_call" && item.CallID != "" {
					calls = append(calls, toolCallInfo{
						CallID:    item.CallID,
						Name:      item.Name,
						Arguments: item.Arguments,
					})
				}
			}
		}
	}
	return calls
}
