# Архитектура GPT Live для Яндекс Станции

В этом документе детально описано внутреннее устройство проекта, интеграция с прошивкой Яндекс Станции (Quasar), организация WebRTC-канала и механизм работы с интернетом и громкостью.

```mermaid
flowchart TD
    subgraph Yandex Station Hardware
        MIC["Массив микрофонов (ALSA)"]
        SPK["Динамик колонки (ALSA)"]
        VOL_SH["/system/vendor/quasar/set_volume.sh"]
    end

    subgraph Native Firmware (Quasar)
        ALICE_SPOTTER["Споттер 'Алиса'"]
        ALICE_CORE["Службы Алисы (Quasar)"]
    end

    subgraph GPT Live Client (Linux ARMv7)
        YANDEX_SPOTTER["Споттер 'Яндекс' (libdecode.so)"]
        WEBRTC["WebRTC Audio & DataChannel Engine (Pion)"]
        AUDIO_BOOST["Аудио-нормализатор (+5 dB Gain Boost)"]
        INTERCEPTOR["Анализатор речи пользователя (Intent Parser)"]
        SEARCH_ENGINE["Локальный поисковый движок"]
    end

    subgraph External Internet Services
        DDG["DuckDuckGo (HTML POST)"]
        CBR["Центробанк РФ (Курсы валют)"]
        BINANCE["Binance API (Криптовалюты)"]
        WTTR["wttr.in (Погода)"]
        WIKI["Wikipedia REST API"]
    end

    subgraph OpenAI Cloud
        OPENAI_STT["OpenAI Speech-to-Text"]
        GPT_CORE["ChatGPT Reasoning Core"]
        OPENAI_TTS["OpenAI Voice Synthesis (Opus)"]
    end

    MIC -->|PCM 16kHz| YANDEX_SPOTTER
    MIC -->|PCM 16kHz| ALICE_SPOTTER

    ALICE_SPOTTER -->|"Слово 'Алиса'"| ALICE_CORE
    ALICE_CORE --> SPK

    YANDEX_SPOTTER -->|"Слово 'Яндекс'"| WEBRTC
    WEBRTC <-->|"WebRTC RTP (Opus 24/48kHz)"| OPENAI_TTS
    WEBRTC -->|"WebRTC DataChannel"| OPENAI_STT

    OPENAI_STT -->|"chat_message_delta (текст речи)"| INTERCEPTOR

    INTERCEPTOR -->|"Команда громкости ('тише' / 'громче')"| VOL_SH
    VOL_SH --> SPK

    INTERCEPTOR -->|"Поисковый запрос (новости / погода / курсы)"| SEARCH_ENGINE
    SEARCH_ENGINE --> DDG
    SEARCH_ENGINE --> CBR
    SEARCH_ENGINE --> BINANCE
    SEARCH_ENGINE --> WTTR
    SEARCH_ENGINE --> WIKI

    SEARCH_ENGINE -->|"relay_message (Актуальные факты)"| WEBRTC
    WEBRTC -->|"data_message / relay_message"| GPT_CORE

    OPENAI_TTS -->|"Голосовой ответ"| WEBRTC
    WEBRTC --> AUDIO_BOOST
    AUDIO_BOOST -->|PCM 48kHz| SPK
```

---

## 1. Сосуществование с Алисой (Dual Wake Word)

На Станции работает нативный аудиодекодер Яндекса (`libdecode.so`).
- В приложении «Дом с Алисой» имя колонки устанавливается в **«Алиса»**.
- Штатные службы Quasar реагируют на слово «Алиса» и воспроизводят стандартные ответы, музыку Яндекса и умный дом.
- Наш фоновый демон `client` загружает нативную библиотеку споттера с конфигурацией на слово **«Яндекс»**.
- Когда произносится «Яндекс»:
  1. Демон подаёт короткий звуковой сигнал готовности (880 Гц).
  2. Временно перехватывает воспроизведение.
  3. Инициирует WebRTC-сессию с голосовым шлюзом ChatGPT.
  4. После завершения диалога (по паузе или фразе прощания) колонка подаёт низкий сигнал (440 Гц) и возвращается в режим ожидания споттера.

---

## 2. WebRTC Live Voice Engine

Связь с сервером OpenAI строится по протоколу **ChatGPT Web Voice** (`https://chatgpt.com/realtime/wm`):
- **Signaling:** SDP Offer генерируется нативной Go-библиотекой `pion/webrtc/v4` и отправляется через HTTP POST с заголовками авторизации сессии (`Bearer <token>`, `Oai-Device-Id`).
- **Audio Codec:** двусторонний поток сжатия **Opus** через статически скомпилированную библиотеку `libopus` (CGO).
- **DataChannel:** служебный канал обмена JSON-событиями (`chat_message_delta`, `conversation_update`, `usage_update`).
- **Gain Booster (+5 dB):** оригинальный звук моделей OpenAI в Web Voice звучит тише нативного синтеза Яндекса. Встроенный модуль цифровой обработки усиливает PCM сэмплы с мягким клиппингом (`math.MaxInt16`), обеспечивая сочный и разборчивый звук на динамике Станции.

---

## 3. Механизм интернет-поиска и актуальных данных

Поскольку серверная голосовая модель OpenAI не имеет встроенного поиска по сайтам через Web Voice, задача поиска полностью решена на стороне Яндекс Станции:

1. **Захват полного контекста фразы:**
   В отличие от базовых реализаций, наш клиент собирает все патчи `chat_message_delta` до конца хода пользователя, накапливая полное предложение (а не обрывок из первого слова).
2. **Анализ намерения (Intent Parser):**
   Регулярные выражения и ключевые слова мгновенно определяют категорию вопроса:
   - *Курсы валют / акций:* запрос к официальному API Центробанка РФ ([cbr-xml-daily.ru](https://www.cbr-xml-daily.ru/)) и Binance (< 150 мс).
   - *Погода:* парсинг города и запрос к `wttr.in` на русском языке.
   - *Время / дата:* точное локальное московское время.
   - *Свободный поиск / новости / факты:* прямой HTTP `POST` в поисковик **DuckDuckGo** с извлечением заголовков и сниппетов (с авто-фоллбэком на Wikipedia API).
3. **Бесшовная доставка в контекст модели:**
   Клиент оборачивает найденные факты в валидный протокольный пакет `relay_message` (`{"type": "data_message", "data": "..."}`) от имени пользователя.
4. **Очистка буфера воспроизведения (`flushPlayback`):**
   Если модель уже начала говорить заглушку («Сейчас проверю...»), аудио-буфер мгновенно сбрасывается, и модель плавно начинает озвучивать актуальный ответ с учётом найденных данных.

---

## 4. Нативное управление громкостью Станции

Клиент управляет громкостью на уровне операционной системы Linux и железа колонки:
- **Чтение:** чтение сохранённого значения из `/bkupgrade/data/quasar/last_user_volume` или опрос ALSA микшера (`amixer sget Master`).
- **Установка:** запуск системного скрипта `/system/vendor/quasar/set_volume.sh <percent>` с окружением библиотек `LD_LIBRARY_PATH=/system/vendor/quasar/libs`.
- **Шкала:** полная поддержка как процентов (0–100%), так и стандартной яндексовской шкалы от 1 до 10 («громкость 5» = 50%, «громкость 8» = 80%), а также относительных команд («потише» = -15%, «погромче» = +15%, «на максимум» = 100%, «без звука» = 0%).
