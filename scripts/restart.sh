#!/usr/bin/env bash
set -euo pipefail

echo "Перезапуск службы client на Яндекс Станции..."
adb shell "killall client 2>/dev/null; rm -f /tmp/yandex-station-chatgpt-voice/client.pid /data/yandex-station-chatgpt-voice/stopped; sleep 1"
adb shell "sh /data/yandex-station-chatgpt-voice/start.sh"
sleep 2
adb shell "pidof client >/dev/null && echo '[OK] Служба успешно перезапущена!' || echo '[!] Служба не запущена'"
