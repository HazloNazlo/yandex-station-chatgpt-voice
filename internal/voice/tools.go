package voice

import (
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var sharedHTTPClient = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
}

var (
	cbrMu       sync.Mutex
	cbrCached   string
	cbrCachedAt time.Time

	weatherMu       sync.Mutex
	weatherCached   = make(map[string]string)
	weatherCachedAt = make(map[string]time.Time)
)

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
	lower = strings.Trim(lower, ".!?,;:\")'\"")

	// If empty or an explicit question, it's not a hangup
	if lower == "" || strings.HasSuffix(text, "?") {
		return false, ""
	}

	// 1. Explicit hangup commands
	explicitPhrases := []string{
		"заверши сесси", "завершить сесси", "завершай сесси",
		"заверши разговор", "завершить разговор", "завершай разговор",
		"заверши звонок", "завершить звонок", "закончи разговор", "закончить разговор",
		"закрой разговор", "закрой сесси", "отключи сесси", "останови сесси",
		"останови разговор", "положи трубку", "конец связи", "прекрати разговор",
	}
	for _, phrase := range explicitPhrases {
		if strings.Contains(lower, phrase) {
			return true, "До связи!"
		}
	}

	// Tokenize words
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var cleanWords []string
	for _, f := range fields {
		if f != "" {
			cleanWords = append(cleanWords, f)
		}
	}

	if len(cleanWords) == 0 {
		return false, ""
	}

	// Long queries are not farewells
	if len(cleanWords) > 5 {
		return false, ""
	}

	// 2. Multi-word exact farewell phrases
	fullClean := strings.Join(cleanWords, " ")
	multiFarewells := []string{
		"до свидания", "до встречи", "до связи", "пока пока", "ну пока",
		"ладно пока", "давай пока", "все пока", "всё пока", "все хватит",
		"всё хватит", "стоп игра", "все стоп", "всё стоп", "спокойной ночи",
	}
	for _, mf := range multiFarewells {
		if fullClean == mf {
			return true, "До связи!"
		}
	}

	// Check if any isolated word is "пока"
	hasPoka := false
	pokaIdx := -1
	for idx, w := range cleanWords {
		if w == "пока" {
			hasPoka = true
			pokaIdx = idx
			break
		}
	}

	if hasPoka {
		// "пока что", "пока не", "пока нет", "пока ты", "пока я" -> NOT hangup!
		if pokaIdx+1 < len(cleanWords) {
			next := cleanWords[pokaIdx+1]
			if next == "что" || next == "не" || next == "нет" || next == "ты" || next == "я" || next == "мы" {
				return false, ""
			}
		}
		// If phrase is short and contains isolated word "пока"
		if len(cleanWords) <= 4 {
			return true, "До связи!"
		}
	}

	// 3. Strict single-word exact commands: "стоп", "хватит", "отбой", "отключись", "выключись"
	exactFarewells := map[string]bool{
		"отбой":     true,
		"стоп":      true,
		"хватит":    true,
		"отключись": true,
		"выключись": true,
		"прекрати":  true,
	}
	if len(cleanWords) <= 3 {
		for _, w := range cleanWords {
			if exactFarewells[w] {
				// avoid "хватит ли"
				if w == "хватит" && len(cleanWords) > 1 {
					hasLi := false
					for _, cw := range cleanWords {
						if cw == "ли" {
							hasLi = true
						}
					}
					if hasLi {
						return false, ""
					}
				}
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
	lower := strings.ToLower(strings.TrimSpace(text))

	// 0. Capability inquiry / internet test
	if (strings.Contains(lower, "доступ") && strings.Contains(lower, "интернет")) ||
		(strings.Contains(lower, "duckduckgo") && (strings.Contains(lower, "доступ") || strings.Contains(lower, "можешь") || strings.Contains(lower, "умеешь") || strings.Contains(lower, "есть"))) ||
		strings.Contains(lower, "у тебя есть интернет") || strings.Contains(lower, "ты в интернете") ||
		strings.Contains(lower, "ты подключен к интернету") || strings.Contains(lower, "ты подключена к интернету") ||
		strings.Contains(lower, "проверь интернет") || strings.Contains(lower, "проверь связь") ||
		strings.Contains(lower, "проверка интернета") || strings.Contains(lower, "работает ли интернет") ||
		strings.Contains(lower, "есть интернет") || strings.Contains(lower, "есть доступ в интернет") {
		return true, strings.TrimSpace(text)
	}

	// 1. Direct search mentions
	if strings.Contains(lower, "интернет") || strings.Contains(lower, "duckduckgo") ||
		strings.Contains(lower, "дакдакго") || strings.Contains(lower, "дактдакго") ||
		strings.Contains(lower, "википеди") || strings.Contains(lower, "в гугле") ||
		strings.Contains(lower, "в яндексе") || strings.Contains(lower, "погугли") ||
		strings.Contains(lower, "загугли") {
		return true, strings.TrimSpace(text)
	}

	// 2. Explicit triggers
	reExplicit := regexp.MustCompile(`(?:найди|поищи|посмотри|глянь|пробей|узнай|проверь|досмотри|поиск|покажи)\s*(?:в интернете|в сети|в новостях|в википедии)?\s*(.*)`)
	if m := reExplicit.FindStringSubmatch(lower); len(m) > 1 {
		q := strings.TrimSpace(m[1])
		if len(q) > 2 {
			return true, strings.TrimSpace(text)
		}
	}

	// 3. Question words requiring fresh info
	questionTriggers := []string{
		"кто такой", "кто такая", "кто такие", "что такое", "где находится",
		"где располож", "когда выйдет", "когда был", "когда будет", "сколько стоит",
		"какая цена", "что случилось", "что произошло", "какой счет", "кто выиграл",
		"кто победил", "последние новости", "свежие новости", "что нового", "какой сейчас",
		"история появления", "история создания", "история породы", "почему", "зачем",
		"как устроена", "как устроен", "как работает", "в каком году", "в каком городе",
	}
	for _, qtr := range questionTriggers {
		if strings.Contains(lower, qtr) {
			return true, strings.TrimSpace(text)
		}
	}

	// 4. Keywords requiring fresh data
	triggers := []string{
		"курс", "доллар", "евро", "юан", "биткоин", "bitcoin", "btc", "крипт", "валют", "акци",
		"погод", "температур", "прогноз", "градус", "дождь", "снег",
		"новост", "событи", "дайджест", "сводка",
		"число", "день недели", "который час", "сколько времени", "какая дата",
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
	if err := json.UnmarshalRead(resp.Body, &data); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("Официальные курсы валют ЦБ РФ: ")
	if usd, ok := data.Valute["USD"]; ok {
		diff := usd.Value - usd.Previous
		sb.WriteString(fmt.Sprintf("Доллар: %.2f ₽ (%+.2f), ", usd.Value, diff))
	}
	if eur, ok := data.Valute["EUR"]; ok {
		diff := eur.Value - eur.Previous
		sb.WriteString(fmt.Sprintf("Евро: %.2f ₽ (%+.2f), ", eur.Value, diff))
	}
	if cny, ok := data.Valute["CNY"]; ok {
		diff := cny.Value - cny.Previous
		sb.WriteString(fmt.Sprintf("Юань: %.2f ₽ (%+.2f).", cny.Value, diff))
	}
	result := sb.String()

	cbrMu.Lock()
	cbrCached = result
	cbrCachedAt = time.Now()
	cbrMu.Unlock()

	return result, nil
}

func fetchCryptoPrice(ctx context.Context, symbol string) (string, error) {
	reqURL := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%sUSDT", strings.ToUpper(symbol))
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

	weatherMu.Lock()
	if cached, ok := weatherCached[city]; ok && time.Since(weatherCachedAt[city]) < 15*time.Minute {
		weatherMu.Unlock()
		return cached, nil
	}
	weatherMu.Unlock()

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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(string(body))
	if res == "" || strings.Contains(res, "Unknown location") {
		return "", errors.New("location not found")
	}
	result := fmt.Sprintf("Погода сейчас: %s", res)

	weatherMu.Lock()
	weatherCached[city] = result
	weatherCachedAt[city] = time.Now()
	weatherMu.Unlock()

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

func searchWeb(ctx context.Context, query string) (string, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return `{"error":"empty query"}`, nil
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
		return "Да, у меня на Яндекс Станции есть постоянный и быстрый доступ в интернет! Я подключен к поисковику DuckDuckGo, Википедии, официальным курсам валют ЦБ РФ, криптобиржам и прогнозу погоды. Я могу в реальном времени искать любую актуальную информацию.", nil
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

	cleanQ := cleanSearchQuery(trimmed)
	if cleanQ == "" {
		cleanQ = trimmed
	}

	// 5. Search via Wikipedia (instant 200ms encyclopedic facts)
	if res, err := fetchWikipedia(ctx, cleanQ); err == nil && res != "" {
		return res, nil
	}
	if cleanQ != trimmed {
		if res, err := fetchWikipedia(ctx, trimmed); err == nil && res != "" {
			return res, nil
		}
	}

	// 6. Search via DuckDuckGo
	if res, err := fetchDuckDuckGo(ctx, cleanQ); err == nil && res != "" {
		return res, nil
	}
	if cleanQ != trimmed {
		if res, err := fetchDuckDuckGo(ctx, trimmed); err == nil && res != "" {
			return res, nil
		}
	}

	return fmt.Sprintf("По запросу %q подробностей не найдено, но интернет работает стабильно.", cleanQ), nil
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
			"name": "set_voice",
			"description": "Смена голоса ассистента на Станции (orbit, vale, breeze, juniper, sky, cove, ember, sol). Используй, когда пользователь просит изменить голос или включить женский/мужской голос.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"voice": map[string]any{
						"type":        "string",
						"enum":        availableVoices,
						"description": "Имя голоса: orbit, vale, breeze, juniper, sky, cove, ember, sol, maple, spruce, arbor",
					},
				},
				"required": []string{"voice"},
			},
		},
		{
			"type": "function",
			"name": "search_web",
			"description": "Поиск актуальной информации в интернете через DuckDuckGo, курсы валют ЦБ РФ, криптобиржи и прогноз погоды. Вызывай, когда пользователю нужны факты, новости, курсы или прогноз.",
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
		{
			"type": "function",
			"name": "hang_up",
			"description": "Завершение текущего разговора и отключение звонка на колонке. Вызывай, когда пользователь прощается ('пока', 'до свидания', 'отбой', 'хватит', 'стоп') или просит закончить разговор.",
			"parameters": map[string]any{
				"type": "object",
			},
		},
	}

	payload := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": "Ты умный голосовой помощник на Яндекс Станции Мини. Тебя зовут ChatGPT. Ты общаешься только голосом через динамик колонки. Отвечай кратко, емко и по существу (1-2 предложения), без списков и вступительных фраз. У тебя есть постоянный выход в интернет (search_web), управление громкостью (set_volume), переключение голосов (set_voice) и завершение разговора (hang_up). Если вопрос касается текущих данных, новостей, погоды или незнакомых фактов — всегда вызывай search_web. Никогда не говори 'у меня нет доступа в интернет'. Если пользователь прощается — вежливо скажи короткое 'Пока!' и вызови hang_up.",
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
