package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

func newSecureHTTPClient() *http.Client {
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	for _, certPath := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/system/vendor/quasar/certs/ca-certificates.crt",
		"/etc/ssl/cert.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
	} {
		if data, err := os.ReadFile(certPath); err == nil {
			rootCAs.AppendCertsFromPEM(data)
		}
	}

	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: rootCAs,
			},
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

var sharedHTTPClient = newSecureHTTPClient()

func decodeToolJSON(resp *http.Response, limit int64, target any) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tool provider returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return errors.New("tool provider response is truncated or exceeds bounds")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("tool provider returned invalid JSON")
	}
	return nil
}

var (
	cbrMu       sync.Mutex
	cbrCached   string
	cbrCachedAt time.Time

	weatherMu       sync.Mutex
	weatherCached   = make(map[string]string)
	weatherCachedAt = make(map[string]time.Time)
)

func cacheWeather(key, result string) {
	weatherMu.Lock()
	defer weatherMu.Unlock()
	const maxWeatherCacheEntries = 64
	if _, exists := weatherCached[key]; !exists && len(weatherCached) >= maxWeatherCacheEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, stored := range weatherCachedAt {
			if oldest.IsZero() || stored.Before(oldest) {
				oldestKey, oldest = candidate, stored
			}
		}
		delete(weatherCached, oldestKey)
		delete(weatherCachedAt, oldestKey)
	}
	weatherCached[key], weatherCachedAt[key] = result, time.Now()
}

func init() {
	// Configure reliable DNS resolver directly using Yandex DNS (77.88.8.8) and Google DNS (8.8.8.8)
	// to bypass broken Android netd/dnsmasq 127.0.0.1:53 resolver on rooted Yandex Station Mini.
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			servers := []string{
				"77.88.8.8:53", // Yandex primary
				"77.88.8.1:53", // Yandex secondary
				"8.8.8.8:53",   // Google
				"1.1.1.1:53",   // Cloudflare
			}
			var lastErr error
			for _, s := range servers {
				conn, err := d.DialContext(ctx, "udp", s)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}

var (
	volumeMu     sync.Mutex
	cachedVolume = -1

	voiceMu     sync.Mutex
	cachedVoice = ""

	snippetRegex = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	titleRegex   = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]*>(.*?)</a>`)
	tagRegex     = regexp.MustCompile(`<[^>]+>`)
	wsRegex      = regexp.MustCompile(`\s+`)
)

const voiceFile = "/data/yandex-station-chatgpt-voice/voice.name"

var availableVoices = []string{"orbit", "vale", "breeze", "juniper", "sky", "cove", "ember", "sol", "maple", "spruce", "arbor"}

func getSavedVoice() string {
	voiceMu.Lock()
	defer voiceMu.Unlock()

	if cachedVoice != "" {
		return cachedVoice
	}

	if data, err := os.ReadFile(voiceFile); err == nil {
		v := strings.ToLower(strings.TrimSpace(string(data)))
		for _, name := range availableVoices {
			if v == name {
				cachedVoice = v
				return cachedVoice
			}
		}
	}

	cachedVoice = "orbit"
	return cachedVoice
}

func saveVoice(name string) error {
	voiceMu.Lock()
	defer voiceMu.Unlock()

	name = strings.ToLower(strings.TrimSpace(name))
	valid := false
	for _, v := range availableVoices {
		if v == name {
			valid = true
			break
		}
	}
	if !valid {
		name = "orbit"
	}

	cachedVoice = name
	_ = os.MkdirAll("/data/yandex-station-chatgpt-voice", 0755)
	return os.WriteFile(voiceFile, []byte(name+"\n"), 0644)
}

func nextVoice(current string) string {
	for i, v := range availableVoices {
		if v == current {
			return availableVoices[(i+1)%len(availableVoices)]
		}
	}
	return "vale"
}

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

func parseVoiceCommand(text string) (bool, string, string) {
	lower := strings.ToLower(strings.TrimSpace(text))

	// 1. Inquiry about voices: "какие голоса у тебя есть", "какой голос", "список голосов"
	if (strings.Contains(lower, "голос") || strings.Contains(lower, "голоса")) &&
		(strings.Contains(lower, "как") || strings.Contains(lower, "каки") || strings.Contains(lower, "список") ||
			strings.Contains(lower, "вариант") || strings.Contains(lower, "доступн") || strings.Contains(lower, "выбрат") ||
			strings.Contains(lower, "перечисл") || strings.Contains(lower, "какой")) {
		cur := getSavedVoice()
		msg := fmt.Sprintf("Сейчас включен голос %s. Доступны женские голоса: Vale, Juniper, Sky, Sol, Maple, и мужские: Breeze, Orbit, Cove, Ember, Spruce, Arbor. Чтобы переключить, скажи «Смени голос на женский» или назови конкретный голос.", cur)
		return true, "", msg
	}

	// 2. Female voice switch
	if (strings.Contains(lower, "женск") || strings.Contains(lower, "девушк") || strings.Contains(lower, "женщин")) &&
		(strings.Contains(lower, "голос") || strings.Contains(lower, "поставь") || strings.Contains(lower, "включи") || strings.Contains(lower, "смен") || strings.Contains(lower, "с мини")) {
		next := "vale"
		msg := "Голос изменен на женский Vale! Скажи «Яндекс», чтобы продолжить уже с новым голосом."
		return true, next, msg
	}

	// 3. Male voice switch
	if (strings.Contains(lower, "мужск") || strings.Contains(lower, "парн") || strings.Contains(lower, "мужчин")) &&
		(strings.Contains(lower, "голос") || strings.Contains(lower, "поставь") || strings.Contains(lower, "включи") || strings.Contains(lower, "смен") || strings.Contains(lower, "с мини")) {
		next := "breeze"
		msg := "Голос изменен на мужской Breeze! Скажи «Яндекс», чтобы продолжить уже с новым голосом."
		return true, next, msg
	}

	// 4. Specific voice names
	namedVoices := map[string]string{
		"vale": "vale", "вэйл": "vale", "вейл": "vale", "валя": "vale",
		"breeze": "breeze", "бриз": "breeze",
		"orbit": "orbit", "орбит": "orbit",
		"juniper": "juniper", "джунипер": "juniper",
		"sky": "sky", "скай": "sky",
		"cove": "cove", "коув": "cove", "ков": "cove",
		"ember": "ember", "эмбер": "ember",
		"sol": "sol", "сол": "sol",
		"maple": "maple", "мейпл": "maple",
		"spruce": "spruce", "спрус": "spruce",
		"arbor": "arbor", "арбор": "arbor",
	}
	for name, target := range namedVoices {
		if strings.Contains(lower, name) && (strings.Contains(lower, "голос") || strings.Contains(lower, "поставь") || strings.Contains(lower, "включи") || strings.Contains(lower, "выбери") || strings.Contains(lower, "смен") || strings.Contains(lower, "с мини")) {
			msg := fmt.Sprintf("Голос изменен на %s! Скажи «Яндекс», чтобы продолжить с новым голосом.", target)
			return true, target, msg
		}
	}

	// 5. Next / cycle voice
	if (strings.Contains(lower, "смен") || strings.Contains(lower, "помен") || strings.Contains(lower, "переключ") || strings.Contains(lower, "следующ") || strings.Contains(lower, "друг") || strings.Contains(lower, "с мини")) &&
		(strings.Contains(lower, "голос") || strings.Contains(lower, "голоса")) {
		cur := getSavedVoice()
		next := nextVoice(cur)
		msg := fmt.Sprintf("Голос изменен на %s! Скажи «Яндекс», чтобы продолжить с новым голосом.", next)
		return true, next, msg
	}

	return false, "", ""
}

func handleSetVoice(argsJSON string) (string, error) {
	var args struct {
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Voice == "" {
		args.Voice = strings.Trim(strings.TrimSpace(argsJSON), `"{}`)
	}
	voice := strings.ToLower(strings.TrimSpace(args.Voice))
	if voice == "" {
		voice = nextVoice(getSavedVoice())
	}
	if err := saveVoice(voice); err != nil {
		return "", fmt.Errorf("failed to save voice: %w", err)
	}
	return fmt.Sprintf(`{"status":"voice_changed","voice":%q,"message":"Голос изменен на %s. При следующем вызове буду говорить им."}`, voice, voice), nil
}

func parseHangupCommand(text string) (bool, string) {
	lower := strings.ToLower(strings.TrimSpace(text))

	// If empty or an explicit question, it's not a farewell
	if lower == "" || strings.HasSuffix(text, "?") {
		return false, ""
	}

	// Normalize punctuation, ellipses, stutters:
	// e.g. "Завер... Заверши сеанс" -> "завер заверши сеанс"
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return ' '
		}
		return r
	}, lower)
	normalized := strings.Join(strings.Fields(cleaned), " ")
	if normalized == "" {
		return false, ""
	}

	// 1. Explicit hangup / termination commands (exact phrases or prefixes)
	explicitPhrases := []string{
		"заверши сеанс", "завершить сеанс", "завершай сеанс", "завершаем сеанс", "завершаю сеанс",
		"заверши сесси", "завершить сесси", "завершай сесси", "завершаем сесси", "завершаю сесси",
		"заверши разговор", "завершить разговор", "завершай разговор", "завершаем разговор", "завершаю разговор",
		"заверши диалог", "завершить диалог", "завершай диалог", "завершаем диалог", "завершаю диалог",
		"заверши звонок", "завершить звонок", "сбрось звонок", "сбрось вызов",
		"закончи сеанс", "закончить сеанс", "заканчивай сеанс", "заканчиваем сеанс",
		"закончи сесси", "закончить сесси", "заканчивай сесси", "заканчиваем сесси",
		"закончи разговор", "закончить разговор", "заканчивай разговор", "заканчиваем разговор",
		"закончи диалог", "закончить диалог", "заканчивай диалог", "заканчиваем диалог",
		"закрой сеанс", "закрой сесси", "закрой разговор", "закрой диалог",
		"отключи сеанс", "отключи сесси", "отключи разговор", "отключи диалог",
		"останови сеанс", "останови сесси", "останови разговор", "останови диалог",
		"положи трубку", "конец связи", "прекрати разговор",
		"давай прощаться", "пора прощаться", "давай закончим", "давай заканчивать",
		"давай завершим", "давай завершать", "давай отключайся", "давай выключайся",
		"замолчи", "заткнись",
	}
	for _, phrase := range explicitPhrases {
		if strings.Contains(normalized, phrase) {
			return true, "До связи!"
		}
	}

	// Tokenize words
	cleanWords := strings.Fields(normalized)
	if len(cleanWords) == 0 {
		return false, ""
	}

	// Check if sentence starts with question word - if so, it's a question, never a farewell
	firstWord := cleanWords[0]
	if len(cleanWords) > 1 && (firstWord == "а" || firstWord == "и" || firstWord == "ну") {
		firstWord = cleanWords[1]
	}
	questionStarters := map[string]bool{
		"кто": true, "что": true, "где": true, "когда": true, "куда": true,
		"откуда": true, "почему": true, "зачем": true, "сколько": true, "как": true,
		"какой": true, "какая": true, "какое": true, "какие": true, "чей": true,
	}
	if questionStarters[firstWord] {
		return false, ""
	}

	// 2. Strict single-word termination commands
	if len(cleanWords) == 1 {
		singleFarewells := map[string]bool{
			"пока":       true,
			"стоп":       true,
			"хватит":     true,
			"отбой":      true,
			"отключись":  true,
			"выключись":  true,
			"отключайся": true,
			"выключайся": true,
			"закройся":   true,
			"заканчивай": true,
			"завершай":   true,
			"прекрати":   true,
		}
		if singleFarewells[cleanWords[0]] {
			return true, "До связи!"
		}
	}

	// 3. Multi-word exact farewell phrases
	multiFarewells := []string{
		"до свидания", "до встречи", "до связи", "пока пока", "ну пока",
		"ладно пока", "давай пока", "все пока", "всё пока", "ну все пока", "ну всё пока",
		"все хватит", "всё хватит", "стоп игра", "все стоп", "всё стоп",
		"спокойной ночи", "хорошо пока", "пока друг", "пока колонка",
		"всего хорошего", "всего доброго", "пока алиса", "пока gpt", "пока чатгпт",
	}
	for _, mf := range multiFarewells {
		if normalized == mf {
			return true, "До связи!"
		}
	}

	// 4. Structural rule for "пока":
	// In Russian, "пока" is a farewell ONLY if:
	// a) It is the last word in a short farewell expression (<= 3 words):
	//    e.g. "ладно давай пока", "ну всё пока", "хорошо давай пока"
	// b) It is followed ONLY by another "пока" or an assistant name/address:
	//    e.g. "пока чатгпт", "пока gpt", "пока колонка", "пока пока"
	// If "пока" has any other words after it (verbs, adverbs, objects, pronouns),
	// it is an adverb/conjunction ("while / meanwhile / for now / so far") and NOT a farewell!
	pokaIdx := -1
	for idx, w := range cleanWords {
		if w == "пока" {
			pokaIdx = idx
			break
		}
	}

	if pokaIdx >= 0 && len(cleanWords) <= 3 {
		// If "пока" is the last word in the phrase
		if pokaIdx == len(cleanWords)-1 {
			// e.g. "давай пока", "ну пока", "ладно пока", "всё пока", "ну всё пока"
			leadWord := cleanWords[0]
			if leadWord == "давай" || leadWord == "ну" || leadWord == "ладно" ||
				leadWord == "все" || leadWord == "всё" || leadWord == "хорошо" {
				return true, "До связи!"
			}
		} else if pokaIdx == 0 && len(cleanWords) == 2 {
			// "пока" is the first word and followed by one word:
			nextWord := cleanWords[1]
			if nextWord == "пока" || nextWord == "чатгпт" || nextWord == "gpt" ||
				nextWord == "колонка" || nextWord == "друг" || nextWord == "алиса" {
				return true, "До связи!"
			}
		}
	}

	return false, ""
}

func cleanSearchQuery(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))

	cleanPunct := strings.Map(func(r rune) rune {
		if r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':' ||
			r == '"' || r == '\'' || r == '(' || r == ')' || r == '«' || r == '»' {
			return ' '
		}
		return r
	}, lower)

	fillers := []string{
		"посмотри в интернете", "посмотри в сети", "посмотри в википедии",
		"найди в интернете", "найди в сети", "найди в гугле", "найди в яндексе",
		"поищи в интернете", "поищи в сети", "поищи в википедии",
		"узнай в интернете", "узнай в сети", "узнай в википедии",
		"проверь в интернете", "проверь в сети",
		"в интернете", "в википедии", "в сети", "в гугле", "в яндексе", "в duckduckgo",
		"расскажи мне пожалуйста", "расскажи мне", "расскажи пожалуйста",
		"скажи мне пожалуйста", "скажи мне", "скажи пожалуйста",
		"найди мне пожалуйста", "найди мне", "найди пожалуйста",
		"поищи пожалуйста", "посмотри пожалуйста", "покажи пожалуйста",
		"узнай пожалуйста", "проверь пожалуйста",
		"и расскажи", "и покажи", "а покажи", "пожалуйста",
		"расскажи", "скажи", "найди", "поищи", "посмотри", "узнай", "покажи",
	}

	res := cleanPunct
	for _, f := range fillers {
		res = strings.ReplaceAll(res, f, " ")
	}

	ws := regexp.MustCompile(`\s+`)
	res = strings.TrimSpace(ws.ReplaceAllString(res, " "))
	return res
}

func detectSearchQuery(text string) (bool, string) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return false, ""
	}

	fullClean := strings.Join(fields, " ")

	// Filter out small talk and greetings
	smallTalkPhrases := []string{
		"привет", "приветствую", "здравствуй", "здравствуйте", "доброе утро",
		"добрый день", "добрый вечер", "доброй ночи", "как дела", "как твои дела",
		"как поживаешь", "как жизнь", "как настроение", "как сам", "ты кто",
		"как тебя зовут", "спасибо", "большое спасибо", "понял", "понятно",
		"ясно", "хорошо", "ладно", "отлично", "круто", "супер", "окей",
	}
	for _, st := range smallTalkPhrases {
		if fullClean == st {
			return false, ""
		}
	}

	// 1. Explicit search commands: user explicitly asks to look up on internet / web / wiki / google / yandex
	explicitSearchMarkers := []string{
		"в интернете", "в сети", "в википедии", "в гугле", "в яндексе", "в duckduckgo",
		"погугли", "загугли",
		"найди в инете", "поищи в инете", "посмотри в инете",
	}
	for _, marker := range explicitSearchMarkers {
		if strings.Contains(lower, marker) {
			return true, trimmed
		}
	}

	// 2. Direct internet connectivity inquiries
	if (strings.Contains(lower, "доступ") && strings.Contains(lower, "интернет")) ||
		strings.Contains(lower, "у тебя есть интернет") || strings.Contains(lower, "ты в интернете") ||
		strings.Contains(lower, "ты подключен к интернету") || strings.Contains(lower, "ты подключена к интернету") ||
		strings.Contains(lower, "проверь интернет") || strings.Contains(lower, "проверь связь") ||
		strings.Contains(lower, "есть интернет") || strings.Contains(lower, "есть доступ в сеть") ||
		strings.Contains(lower, "работает ли интернет") || strings.Contains(lower, "интернет работает") {
		return true, trimmed
	}

	// 3. Real-time dynamic topics (rates, crypto, live weather, current date/time)
	// Currency / Forex:
	if strings.Contains(lower, "курс") ||
		((strings.Contains(lower, "доллар") || strings.Contains(lower, "евро") || strings.Contains(lower, "юан") || strings.Contains(lower, "рубл")) &&
			(strings.Contains(lower, "почем") || strings.Contains(lower, "сколько стоит") || strings.Contains(lower, "цена") || strings.Contains(lower, "стоимость"))) {
		return true, trimmed
	}

	// Crypto:
	if strings.Contains(lower, "биткоин") || strings.Contains(lower, "bitcoin") || strings.Contains(lower, "btc") ||
		strings.Contains(lower, "криптовалют") || strings.Contains(lower, "крипт") || strings.Contains(lower, "эфириум") || strings.Contains(lower, "eth") {
		return true, trimmed
	}

	// Weather:
	if strings.Contains(lower, "погод") || strings.Contains(lower, "прогноз") ||
		strings.Contains(lower, "будет ли дождь") || strings.Contains(lower, "будет ли снег") ||
		strings.Contains(lower, "температура на улице") || strings.Contains(lower, "градусов на улице") ||
		strings.Contains(lower, "какая температура") {
		return true, trimmed
	}

	// Current date / time:
	if strings.Contains(lower, "который час") || strings.Contains(lower, "сколько времени") ||
		strings.Contains(lower, "сколько сейчас времени") || strings.Contains(lower, "какое сегодня число") ||
		strings.Contains(lower, "какой сегодня день") || strings.Contains(lower, "какая сегодня дата") {
		return true, trimmed
	}

	// 4. News / current events
	if strings.Contains(lower, "новост") || strings.Contains(lower, "что нового") || strings.Contains(lower, "что произошло") {
		return true, trimmed
	}

	// 5. Direct informational queries (encyclopedic, biographical, scientific)
	factPrefixes := []string{
		"кто такой", "кто такая", "кто такие", "кто президент", "кто написал", "кто автор",
		"что такое", "что значит", "где находится", "где расположен", "где расположена",
		"когда родился", "когда родилась", "когда умер", "когда произошл",
		"расскажи историю", "объясни как работает",
	}
	for _, fp := range factPrefixes {
		if strings.Contains(lower, fp) {
			return true, trimmed
		}
	}

	// 6. URL / web page opening
	if isURL, _ := detectURLQuery(trimmed); isURL {
		return true, trimmed
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

func rubleStr(n int) string {
	r10 := n % 10
	r100 := n % 100
	if r10 == 1 && r100 != 11 {
		return fmt.Sprintf("%d рубль", n)
	}
	if r10 >= 2 && r10 <= 4 && (r100 < 10 || r100 >= 20) {
		return fmt.Sprintf("%d рубля", n)
	}
	return fmt.Sprintf("%d рублей", n)
}

func kopeckStr(n int) string {
	r10 := n % 10
	r100 := n % 100
	if r10 == 1 && r100 != 11 {
		return fmt.Sprintf("%d копейка", n)
	}
	if r10 >= 2 && r10 <= 4 && (r100 < 10 || r100 >= 20) {
		return fmt.Sprintf("%d копейки", n)
	}
	return fmt.Sprintf("%d копеек", n)
}

func fetchCBRRates(ctx context.Context) (string, error) {
	cbrMu.Lock()
	if cbrCached != "" && time.Since(cbrCachedAt) < 30*time.Minute {
		res := cbrCached
		cbrMu.Unlock()
		return res, nil
	}
	cbrMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cbr-xml-daily.ru/daily_json.js", nil)
	if err != nil {
		return "", err
	}
	resp, err := sharedHTTPClient.Do(req)
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
	if err := decodeToolJSON(resp, 64<<10, &data); err != nil {
		return "", err
	}
	validAt, err := time.Parse(time.RFC3339, data.Date)
	if err != nil {
		return "", errors.New("CBR mirror returned no valid publication date")
	}
	var rates []string
	for _, currency := range []struct{ code, name string }{{"USD", "доллар"}, {"EUR", "евро"}, {"CNY", "юань"}} {
		rate, ok := data.Valute[currency.code]
		if !ok || rate.Nominal <= 0 || rate.Value <= 0 || math.IsNaN(rate.Value) || math.IsInf(rate.Value, 0) {
			return "", errors.New("CBR mirror returned incomplete or invalid currency data")
		}
		value := rate.Value / float64(rate.Nominal)
		if value > 1e6 {
			return "", errors.New("CBR currency value exceeds supported bounds")
		}
		kopecks := int(math.Round(value * 100))
		rates = append(rates, fmt.Sprintf("%s %s %s", currency.name, rubleStr(kopecks/100), kopeckStr(kopecks%100)))
	}
	result := fmt.Sprintf("Справочный курс ЦБ на %s: %s. Это не курс покупки или продажи в банке.",
		validAt.Format("02.01.2006"), strings.Join(rates, ", "))

	cbrMu.Lock()
	cbrCached = result
	cbrCachedAt = time.Now()
	cbrMu.Unlock()

	return result, nil
}

func fetchCryptoPrice(ctx context.Context, symbol string) (string, error) {
	symbol = strings.ToUpper(symbol)
	name := map[string]string{"BTC": "биткоин", "ETH": "эфир"}[symbol]
	if name == "" {
		return "", errors.New("unsupported crypto ticker")
	}
	reqURL := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%sUSDT", symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := decodeToolJSON(resp, 16<<10, &data); err != nil {
		return "", err
	}
	price, err := strconv.ParseFloat(data.Price, 64)
	if err != nil || data.Symbol != symbol+"USDT" || price <= 0 || price > 1e9 || math.IsNaN(price) || math.IsInf(price, 0) {
		return "", errors.New("Binance returned an invalid quote")
	}
	received := time.Now().In(time.FixedZone("MSK", 3*3600)).Format("15:04")
	return fmt.Sprintf("На Binance один %s стоит %.2f USDT. Котировка получена в %s по московскому времени. USDT не равен доллару один к одному гарантированно.",
		name, price, received), nil
}

func formatWeatherSpeech(city, raw string) string {
	if raw == "" || strings.Contains(raw, "location not found") || strings.Contains(raw, "upstream error") ||
		strings.Contains(raw, "Unknown location") || strings.Contains(raw, "<html>") {
		return ""
	}

	cityName := "вашем городе"
	if city != "" {
		cityName = "городе " + strings.Title(city)
	}

	cond := ""
	if strings.Contains(raw, "☀️") {
		cond = "ясно, "
	} else if strings.Contains(raw, "🌤") || strings.Contains(raw, "⛅") || strings.Contains(raw, "🌥") {
		cond = "переменная облачность, "
	} else if strings.Contains(raw, "☁") {
		cond = "пасмурно, "
	} else if strings.Contains(raw, "🌧") || strings.Contains(raw, "🌦") {
		cond = "небольшой дождь, "
	} else if strings.Contains(raw, "❄") || strings.Contains(raw, "🌨") {
		cond = "снег, "
	} else if strings.Contains(raw, "⛈") {
		cond = "гроза, "
	} else if strings.Contains(raw, "🌫") {
		cond = "туман, "
	}

	tempRe := regexp.MustCompile(`([+-]?\d+)\s*°C`)
	tempStr := ""
	if m := tempRe.FindStringSubmatch(raw); len(m) > 1 {
		val, err := strconv.Atoi(m[1])
		if err == nil {
			if val > 0 {
				tempStr = fmt.Sprintf("плюс %d", val)
			} else if val < 0 {
				tempStr = fmt.Sprintf("минус %d", -val)
			} else {
				tempStr = "ноль"
			}
			absVal := val
			if absVal < 0 {
				absVal = -absVal
			}
			rem10 := absVal % 10
			rem100 := absVal % 100
			if rem10 == 1 && rem100 != 11 {
				tempStr += " градус"
			} else if rem10 >= 2 && rem10 <= 4 && (rem100 < 10 || rem100 >= 20) {
				tempStr += " градуса"
			} else {
				tempStr += " градусов"
			}
		}
	}

	windRe := regexp.MustCompile(`(\d+)\s*m/s`)
	windStr := ""
	if m := windRe.FindStringSubmatch(raw); len(m) > 1 {
		windVal, _ := strconv.Atoi(m[1])
		rem10 := windVal % 10
		rem100 := windVal % 100
		suffix := "метров"
		if rem10 == 1 && rem100 != 11 {
			suffix = "метр"
		} else if rem10 >= 2 && rem10 <= 4 && (rem100 < 10 || rem100 >= 20) {
			suffix = "метра"
		}
		windStr = fmt.Sprintf(", ветер %d %s в секунду", windVal, suffix)
	}

	if tempStr != "" {
		return fmt.Sprintf("Сейчас в %s %s%s%s.", cityName, cond, tempStr, windStr)
	}
	return ""
}

func wmoCodeToString(code int) string {
	switch code {
	case 0:
		return "ясно"
	case 1:
		return "в основном ясно"
	case 2:
		return "переменная облачность"
	case 3:
		return "пасмурно"
	case 45, 48:
		return "туман"
	case 51, 53, 55:
		return "моросящий дождь"
	case 56, 57:
		return "ледяная морось"
	case 61:
		return "небольшой дождь"
	case 63:
		return "умеренный дождь"
	case 65:
		return "сильный дождь"
	case 66, 67:
		return "ледяной дождь"
	case 71:
		return "небольшой снег"
	case 73:
		return "снегопад"
	case 75:
		return "сильный снегопад"
	case 77:
		return "снежные зерна"
	case 80, 81, 82:
		return "ливень"
	case 85, 86:
		return "снежный заряд"
	case 95:
		return "гроза"
	case 96, 99:
		return "гроза с градом"
	default:
		return "облачно"
	}
}

func formatTempSpeech(temp float64) string {
	t := int(math.Round(temp))
	if t > 0 {
		return fmt.Sprintf("плюс %d %s", t, degreeStr(t))
	} else if t < 0 {
		return fmt.Sprintf("минус %d %s", -t, degreeStr(-t))
	}
	return "ноль градусов"
}

func degreeStr(n int) string {
	n = n % 100
	if n >= 11 && n <= 19 {
		return "градусов"
	}
	switch n % 10 {
	case 1:
		return "градус"
	case 2, 3, 4:
		return "градуса"
	default:
		return "градусов"
	}
}

func meterStr(n int) string {
	n = n % 100
	if n >= 11 && n <= 19 {
		return "метров"
	}
	switch n % 10 {
	case 1:
		return "метр"
	case 2, 3, 4:
		return "метра"
	default:
		return "метров"
	}
}

func validWeatherTemperature(value float64) bool {
	return value >= -100 && value <= 70 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validWMOCode(code int) bool {
	switch code {
	case 0, 1, 2, 3, 45, 48, 51, 53, 55, 56, 57, 61, 63, 65, 66, 67, 71, 73, 75, 77, 80, 81, 82, 85, 86, 95, 96, 99:
		return true
	default:
		return false
	}
}

func requestedWeatherDay(query string) (int, error) {
	lower := strings.ToLower(query)
	if strings.Contains(lower, "послезавтра") {
		return 2, nil
	}
	if strings.Contains(lower, "завтра") {
		return 1, nil
	}
	for _, term := range []string{"вчера", "прошл", "следующ", "недел", "выходн", "месяц"} {
		if strings.Contains(lower, term) {
			return 0, errors.New("weather gateway supports current weather, tomorrow, or the day after tomorrow only")
		}
	}
	return 0, nil
}

func fetchOpenMeteoWeather(ctx context.Context, city string, query string) (string, error) {
	day, err := requestedWeatherDay(query)
	if err != nil {
		return "", err
	}
	geoCity := city
	if geoCity == "" {
		geoCity = "Москва"
	}

	geoURL := "https://geocoding-api.open-meteo.com/v1/search?name=" + url.QueryEscape(geoCity) + "&count=1&language=ru&format=json"
	geoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, geoURL, nil)
	if err != nil {
		return "", err
	}
	geoReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	geoResp, err := sharedHTTPClient.Do(geoReq)
	if err != nil {
		return "", err
	}
	defer geoResp.Body.Close()

	var geoData struct {
		Results []struct {
			Name      string   `json:"name"`
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
			Country   string   `json:"country"`
		} `json:"results"`
	}
	if err := decodeToolJSON(geoResp, 64<<10, &geoData); err != nil || len(geoData.Results) == 0 {
		return "", errors.New("geocoding failed")
	}

	target := geoData.Results[0]
	if target.Name == "" || target.Latitude == nil || target.Longitude == nil ||
		math.IsNaN(*target.Latitude) || math.IsNaN(*target.Longitude) ||
		*target.Latitude < -90 || *target.Latitude > 90 || *target.Longitude < -180 || *target.Longitude > 180 {
		return "", errors.New("geocoding returned invalid coordinates")
	}
	cityName := target.Name

	weatherURL := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current=temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m&daily=weather_code,temperature_2m_max,temperature_2m_min&wind_speed_unit=ms&timezone=auto", *target.Latitude, *target.Longitude)
	wReq, err := http.NewRequestWithContext(ctx, http.MethodGet, weatherURL, nil)
	if err != nil {
		return "", err
	}
	wReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	wResp, err := sharedHTTPClient.Do(wReq)
	if err != nil {
		return "", err
	}
	defer wResp.Body.Close()

	var wData struct {
		Current struct {
			Temperature *float64 `json:"temperature_2m"`
			WeatherCode *int     `json:"weather_code"`
			WindSpeed   *float64 `json:"wind_speed_10m"`
			Time        string   `json:"time"`
		} `json:"current"`
		Daily struct {
			Time        []string   `json:"time"`
			WeatherCode []*int     `json:"weather_code"`
			TempMax     []*float64 `json:"temperature_2m_max"`
			TempMin     []*float64 `json:"temperature_2m_min"`
		} `json:"daily"`
	}
	if err := decodeToolJSON(wResp, 128<<10, &wData); err != nil {
		return "", err
	}

	if day > 0 {
		daily := wData.Daily
		if len(daily.Time) <= day || len(daily.WeatherCode) <= day || len(daily.TempMax) <= day || len(daily.TempMin) <= day ||
			daily.WeatherCode[day] == nil || daily.TempMax[day] == nil || daily.TempMin[day] == nil {
			return "", errors.New("Open-Meteo returned an incomplete daily forecast")
		}
		if _, err := time.Parse("2006-01-02", daily.Time[day]); err != nil || !validWMOCode(*daily.WeatherCode[day]) ||
			!validWeatherTemperature(*daily.TempMax[day]) || !validWeatherTemperature(*daily.TempMin[day]) {
			return "", errors.New("Open-Meteo returned an invalid daily forecast")
		}
		desc := wmoCodeToString(*daily.WeatherCode[day])
		tMax := formatTempSpeech(*daily.TempMax[day])
		tMin := formatTempSpeech(*daily.TempMin[day])
		return fmt.Sprintf("По прогнозу Open-Meteo на %s в городе %s ожидается %s, днем до %s, ночью %s.", daily.Time[day], cityName, desc, tMax, tMin), nil
	}

	current := wData.Current
	observed, err := time.Parse("2006-01-02T15:04", current.Time)
	if err != nil || current.Temperature == nil || current.WeatherCode == nil || current.WindSpeed == nil ||
		!validWeatherTemperature(*current.Temperature) || !validWMOCode(*current.WeatherCode) ||
		*current.WindSpeed < 0 || *current.WindSpeed > 150 || math.IsNaN(*current.WindSpeed) || math.IsInf(*current.WindSpeed, 0) {
		return "", errors.New("Open-Meteo returned incomplete or invalid current weather")
	}
	desc := wmoCodeToString(*current.WeatherCode)
	temp := formatTempSpeech(*current.Temperature)
	wind := int(math.Round(*current.WindSpeed))
	return fmt.Sprintf("По данным Open-Meteo на %s %s по местному времени, в городе %s %s, %s, ветер %d %s в секунду.",
		observed.Format("02.01.2006"), observed.Format("15:04"), cityName, desc, temp, wind, meterStr(wind)), nil
}

var russianCityNominative = map[string]string{
	"москве": "Москва", "москва": "Москва",
	"калуге": "Калуга", "калуга": "Калуга",
	"питере": "Санкт-Петербург", "петербурге": "Санкт-Петербург", "санкт-петербурге": "Санкт-Петербург", "спб": "Санкт-Петербург", "санкт петербурге": "Санкт-Петербург",
	"самаре": "Самара", "самара": "Самара",
	"казани": "Казань", "казань": "Казань",
	"уфе": "Уфа", "уфа": "Уфа",
	"туле": "Тула", "тула": "Тула",
	"твери": "Тверь", "тверь": "Тверь",
	"перми": "Пермь", "пермь": "Пермь",
	"сочи":       "Сочи",
	"краснодаре": "Краснодар", "краснодар": "Краснодар",
	"ростове": "Ростов-на-Дону", "ростов": "Ростов-на-Дону", "ростове на дону": "Ростов-на-Дону",
	"воронеже": "Воронеж", "воронеж": "Воронеж",
	"екатеринбурге": "Екатеринбург", "екатеринбург": "Екатеринбург",
	"новосибирске": "Новосибирск", "новосибирск": "Новосибирск",
	"челябинске": "Челябинск", "челябинск": "Челябинск",
	"омске": "Омск", "омск": "Омск",
	"красноярске": "Красноярск", "красноярск": "Красноярск",
	"волгограде": "Волгоград", "волгоград": "Волгоград",
	"саратове": "Саратов", "саратов": "Саратов",
	"тюмени": "Тюмень", "тюмень": "Тюмень",
	"тольятти": "Тольятти",
	"ижевске":  "Ижевск", "ижевск": "Ижевск",
	"барнауле": "Барнаул", "барнаул": "Барнаул",
	"иркутске": "Иркутск", "иркутск": "Иркутск",
	"ульяновске": "Ульяновск", "ульяновск": "Ульяновск",
	"хабаровске": "Хабаровск", "хабаровск": "Хабаровск",
	"владивостоке": "Владивосток", "владивосток": "Владивосток",
	"ярославле": "Ярославль", "ярославль": "Ярославль",
	"томске": "Томск", "томск": "Томск",
	"оренбурге": "Оренбург", "оренбург": "Оренбург",
	"рязани": "Рязань", "рязань": "Рязань",
	"астрахани": "Астрахань", "астрахань": "Астрахань",
	"пензе": "Пенза", "пенза": "Пенза",
	"липецке": "Липецк", "липецк": "Липецк",
	"кирове": "Киров", "киров": "Киров",
	"чебоксарах": "Чебоксары", "чебоксары": "Чебоксары",
	"калининграде": "Калининград", "калининград": "Калининград",
	"курске": "Курск", "курск": "Курск",
	"брянске": "Брянск", "брянск": "Брянск",
	"орле": "Орёл", "орел": "Орёл",
	"белгороде": "Белгород", "белгород": "Белгород",
	"владимире": "Владимир", "владимир": "Владимир",
	"смоленске": "Смоленск", "смоленск": "Смоленск",
	"иванове": "Иваново", "иваново": "Иваново",
	"нижнем новгороде":  "Нижний Новгород",
	"великом новгороде": "Великий Новгород",
}

func isWeatherStopword(w string) bool {
	stopwords := map[string]bool{
		"сейчас": true, "сегодня": true, "завтра": true, "послезавтра": true,
		"утром": true, "вечером": true, "днем": true, "ночью": true,
		"на": true, "в": true, "во": true, "для": true, "пожалуйста": true,
		"мне": true, "нам": true, "подскажи": true, "скажи": true, "будет": true,
		"ожидается": true, "какая": true, "какой": true, "погода": true, "погоду": true,
		"температура": true, "температуру": true, "градусов": true, "градус": true,
		"неделе": true, "выходных": true, "там": true, "тут": true, "здесь": true,
	}
	return stopwords[w]
}

func extractCityFromWeather(query string) string {
	lower := strings.ToLower(query)
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return ' '
		}
		return r
	}, lower)
	words := strings.Fields(cleaned)

	cityWords := []string{}
	foundPrep := false
	for _, w := range words {
		if !foundPrep {
			if w == "в" || w == "во" || w == "для" || w == "городе" || w == "город" {
				foundPrep = true
			}
			continue
		}
		if isWeatherStopword(w) {
			if len(cityWords) > 0 {
				break
			}
			continue
		}
		cityWords = append(cityWords, w)
		if len(cityWords) == 2 {
			break
		}
	}

	if len(cityWords) == 0 {
		return ""
	}
	return strings.Join(cityWords, " ")
}

func normalizeCity(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return "Москва"
	}
	if nom, ok := russianCityNominative[raw]; ok {
		return nom
	}
	if strings.Contains(raw, "петербург") || strings.Contains(raw, "питер") {
		return "Санкт-Петербург"
	}
	if strings.Contains(raw, "новгород") {
		if strings.Contains(raw, "нижн") {
			return "Нижний Новгород"
		}
		if strings.Contains(raw, "велик") {
			return "Великий Новгород"
		}
	}
	if strings.Contains(raw, "ростов") {
		return "Ростов-на-Дону"
	}
	runes := []rune(raw)
	if len(runes) > 3 && runes[len(runes)-1] == 'е' {
		candidate := string(runes[:len(runes)-1]) + "а"
		if nom, ok := russianCityNominative[candidate]; ok {
			return nom
		}
		return strings.Title(candidate)
	}
	return strings.Title(raw)
}

func fetchWeather(ctx context.Context, query string) (string, error) {
	day, err := requestedWeatherDay(query)
	if err != nil {
		return "", err
	}
	rawCity := extractCityFromWeather(query)
	city := normalizeCity(rawCity)

	weatherMu.Lock()
	cacheKey := city + "_" + query
	if cached, ok := weatherCached[cacheKey]; ok && time.Since(weatherCachedAt[cacheKey]) < 15*time.Minute {
		weatherMu.Unlock()
		return cached, nil
	}
	weatherMu.Unlock()

	// 1. Primary provider: Open-Meteo with geocoding (fast 1.5s timeout)
	omCtx, omCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	res, omErr := fetchOpenMeteoWeather(omCtx, city, query)
	omCancel()
	if omErr == nil && res != "" {
		cacheWeather(cacheKey, res)
		return res, nil
	}

	if day > 0 {
		return "", errors.New("forecast provider unavailable; current wttr.in weather cannot answer a future date")
	}
	// Fallback is current weather only and must be identified as such.
	reqURL := "https://wttr.in/" + url.PathEscape(city) + "?m&M&format=4&lang=ru"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/7.88.1")
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather fallback returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil || len(body) > 1024 {
		return "", errors.New("weather fallback response is truncated or exceeds bounds")
	}
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr == "" || strings.Contains(bodyStr, "Unknown location") || strings.Contains(bodyStr, "location not found") {
		return "", errors.New("location not found")
	}
	result := formatWeatherSpeech(city, bodyStr)
	if result == "" {
		return "", errors.New("cannot format weather response")
	}
	result = fmt.Sprintf("Данные wttr.in, полученные %s по московскому времени. %s",
		time.Now().In(time.FixedZone("MSK", 3*3600)).Format("02.01.2006 15:04"), result)
	cacheWeather(cacheKey, result)

	return result, nil
}

func fetchWikipedia(ctx context.Context, query string) (string, error) {
	reqURL := "https://ru.wikipedia.org/w/api.php?action=query&list=search&srsearch=" + url.QueryEscape(query) + "&utf8=&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := sharedHTTPClient.Do(req)
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

	resp, err := sharedHTTPClient.Do(req)
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

func isPrivateOrBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0:
			return true
		case ip4[0] == 10:
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			return true
		case ip4[0] == 127:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0:
			return true
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100:
			return true
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113:
			return true
		case ip4[0] >= 224:
			return true
		}
	}
	return false
}

func extractPageContent(htmlStr string) (string, string) {
	reTitle := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	title := ""
	if m := reTitle.FindStringSubmatch(htmlStr); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(cleanHTML(m[1])))
	}

	clean := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(htmlStr, " ")
	clean = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(clean, " ")
	clean = regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`).ReplaceAllString(clean, " ")
	clean = regexp.MustCompile(`(?is)<!--.*?-->`).ReplaceAllString(clean, " ")

	clean = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(clean, " ")
	clean = html.UnescapeString(clean)

	clean = regexp.MustCompile(`\s+`).ReplaceAllString(clean, " ")
	clean = strings.TrimSpace(clean)

	runes := []rune(clean)
	if len(runes) > 350 {
		clean = string(runes[:350]) + "..."
	}

	return title, clean
}

func openURL(ctx context.Context, rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("некорректный адрес ссылки: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("поддерживаются только ссылки http и https")
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", errors.New("пустой хост")
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	if err != nil {
		return "", fmt.Errorf("адрес %s не найден: %w", hostname, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("адрес %s не найден", hostname)
	}
	for _, ip := range ips {
		if isPrivateOrBlockedIP(ip) {
			return "", fmt.Errorf("доступ к приватному или локальному адресу %s заблокирован", ip.String())
		}
	}

	safeClient := &http.Client{
		Timeout: 6 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
				Control: func(network, address string, c syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}
					ip := net.ParseIP(host)
					if isPrivateOrBlockedIP(ip) {
						return fmt.Errorf("SSRF blocked: %s is private", host)
					}
					return nil
				},
			}).DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("слишком много перенаправлений")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("недопустимый протокол перенаправления")
			}
			destHost := req.URL.Hostname()
			destIPs, err := net.LookupIP(destHost)
			if err != nil {
				return err
			}
			for _, ip := range destIPs {
				if isPrivateOrBlockedIP(ip) {
					return fmt.Errorf("перенаправление на приватный адрес заблокировано")
				}
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; YandexStationBot/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8")

	resp, err := safeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка загрузки страницы: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("страница вернула статус %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("ошибка чтения содержимого: %w", err)
	}

	title, readable := extractPageContent(string(bodyBytes))
	if title == "" && readable == "" {
		return fmt.Sprintf("Страница %s открыта, но на ней нет текстового содержимого.", hostname), nil
	}
	if title != "" && readable != "" {
		return fmt.Sprintf("Страница %s. Заголовок: %s. Содержимое: %s", hostname, title, readable), nil
	}
	if title != "" {
		return fmt.Sprintf("Страница %s. Заголовок: %s.", hostname, title), nil
	}
	return fmt.Sprintf("Страница %s. Содержимое: %s", hostname, readable), nil
}

func detectURLQuery(text string) (bool, string) {
	lower := strings.ToLower(strings.TrimSpace(text))

	reURL := regexp.MustCompile(`https?://[^\s"'<>]+`)
	if m := reURL.FindString(text); m != "" {
		return true, m
	}

	reDomain := regexp.MustCompile(`(?:открой|посмотри|прочитай|перейди на|зайди на|что на)\s+(?:сайт[еа]?\s+)?([a-z0-9-]+(?:\.[a-z0-9-]+)+(?:/[^\s]*)?)`)
	if m := reDomain.FindStringSubmatch(lower); len(m) > 1 {
		target := m[1]
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			target = "https://" + target
		}
		return true, target
	}

	return false, ""
}

const defaultTavilyAPIKey = "tvly-3jhput-wv2wreESyyP0ThUse19C2KCQuxZW3z7OlGHZlBiCfx"
const tavilyKeyFile = "/data/yandex-station-chatgpt-voice/tavily.key"

func getTavilyAPIKey() string {
	if k := os.Getenv("TAVILY_API_KEY"); k != "" {
		return strings.TrimSpace(k)
	}
	if data, err := os.ReadFile(tavilyKeyFile); err == nil {
		k := strings.TrimSpace(string(data))
		if k != "" {
			return k
		}
	}
	return defaultTavilyAPIKey
}

func fetchTavilySearch(ctx context.Context, query string) (string, error) {
	apiKey := getTavilyAPIKey()
	if apiKey == "" {
		return "", errors.New("tavily api key is empty")
	}

	searchQ := strings.TrimSpace(query)
	if !strings.Contains(strings.ToLower(searchQ), "русск") {
		searchQ = "ответь на русском языке кратко: " + searchQ
	}

	reqBody := map[string]any{
		"api_key":        apiKey,
		"query":          searchQ,
		"search_depth":   "basic",
		"include_answer": true,
		"max_results":    3,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "YandexStationVoice/1.0")

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tavily returned HTTP %d", resp.StatusCode)
	}

	var data struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"results"`
	}

	if err := decodeToolJSON(resp, 256<<10, &data); err != nil {
		return "", err
	}

	ans := strings.TrimSpace(data.Answer)
	if ans != "" {
		return ans, nil
	}

	var snippets []string
	for _, r := range data.Results {
		c := strings.TrimSpace(r.Content)
		if c != "" {
			snippets = append(snippets, c)
			if len(snippets) >= 2 {
				break
			}
		}
	}
	if len(snippets) > 0 {
		return strings.Join(snippets, " "), nil
	}

	return "", errors.New("tavily returned no usable answer")
}

func searchWeb(ctx context.Context, query string) (string, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return "", errors.New("empty search query")
	}
	lower := strings.ToLower(trimmed)

	// 0. Capability inquiry / connection test
	if (strings.Contains(lower, "доступ") && strings.Contains(lower, "интернет")) ||
		(strings.Contains(lower, "duckduckgo") && (strings.Contains(lower, "доступ") || strings.Contains(lower, "можешь") || strings.Contains(lower, "умеешь") || strings.Contains(lower, "есть"))) ||
		strings.Contains(lower, "у тебя есть интернет") || strings.Contains(lower, "ты в интернете") ||
		strings.Contains(lower, "ты подключен к интернету") || strings.Contains(lower, "ты подключена к интернету") ||
		strings.Contains(lower, "проверь интернет") || strings.Contains(lower, "проверь связь") ||
		strings.Contains(lower, "проверка интернета") || strings.Contains(lower, "работает ли интернет") ||
		strings.Contains(lower, "есть интернет") || strings.Contains(lower, "есть доступ в интернет") {
		return "На Станции настроены локальные источники погоды, справочного курса ЦБ, котировок Binance и умный веб-поиск Tavily. Их доступность проверяется при каждом запросе.", nil
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
		return fetchCryptoPrice(ctx, "BTC")
	}
	if strings.Contains(lower, "эфир") || strings.Contains(lower, "ethereum") || strings.Contains(lower, "eth") {
		return fetchCryptoPrice(ctx, "ETH")
	}

	// 3. Fiat currency query
	if strings.Contains(lower, "доллар") || strings.Contains(lower, "евро") ||
		strings.Contains(lower, "юан") || strings.Contains(lower, "курс валют") ||
		strings.Contains(lower, "курс рубля") || strings.Contains(lower, "какой курс") {
		return fetchCBRRates(ctx)
	}

	// 4. Weather query
	if strings.Contains(lower, "погод") || strings.Contains(lower, "температур") ||
		strings.Contains(lower, "градус") || strings.Contains(lower, "дождь") || strings.Contains(lower, "снег") {
		return fetchWeather(ctx, trimmed)
	}

	// 5. URL opening inquiry
	if isURL, targetURL := detectURLQuery(trimmed); isURL {
		return openURL(ctx, targetURL)
	}

	cleanQ := cleanSearchQuery(trimmed)
	if cleanQ == "" {
		cleanQ = trimmed
	}

	for _, term := range []string{"новост", "свеж", "сегодня", "последн", "недавн", "сейчас", "текущ"} {
		if strings.Contains(lower, term) {
			res, err := fetchDuckDuckGo(ctx, cleanQ)
			if err != nil {
				return "", err
			}
			return "Найдены поисковые выдержки. Дата публикации и факты не проверены независимо. " + res, nil
		}
	}

	// 6. Intelligent web search via Tavily
	if ans, err := fetchTavilySearch(ctx, cleanQ); err == nil && ans != "" {
		return ans, nil
	}
	if cleanQ != trimmed {
		if ans, err := fetchTavilySearch(ctx, trimmed); err == nil && ans != "" {
			return ans, nil
		}
	}

	// Encyclopedia results
	if res, err := fetchWikipedia(ctx, cleanQ); err == nil && res != "" {
		return res, nil
	}
	if cleanQ != trimmed {
		if res, err := fetchWikipedia(ctx, trimmed); err == nil && res != "" {
			return res, nil
		}
	}

	// Fallback via DuckDuckGo
	if res, err := fetchDuckDuckGo(ctx, cleanQ); err == nil && res != "" {
		return res, nil
	}
	if cleanQ != trimmed {
		if res, err := fetchDuckDuckGo(ctx, trimmed); err == nil && res != "" {
			return res, nil
		}
	}

	return "", errors.New("search providers returned no usable results")
}
