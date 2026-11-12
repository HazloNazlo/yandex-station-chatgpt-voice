#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_ROOT/build/yandex-station-chatgpt-voice"

if [ ! -f "$BIN" ]; then
    echo "[ОШИБКА] Бинарник $BIN не найден."
    echo "Соберите его: ./tools/build-arm.sh build"
    exit 1
fi

DEVICE=""
if ! adb get-state >/dev/null 2>&1; then
    read -rp "Введите IP адрес Станции (например, 192.168.1.100): " STATION_IP
    adb connect "$STATION_IP:5555"
    sleep 2
    DEVICE="-s $STATION_IP:5555"
fi

echo "[1/3] Загрузка обновленного бинарника на Станцию..."
adb $DEVICE shell "mkdir -p /data/yandex-station-chatgpt-voice"
adb $DEVICE shell "killall client 2>/dev/null; cp -f /data/yandex-station-chatgpt-voice/client /data/yandex-station-chatgpt-voice/client.previous 2>/dev/null || true"
adb $DEVICE push "$BIN" /data/yandex-station-chatgpt-voice/client
adb $DEVICE shell "chmod 755 /data/yandex-station-chatgpt-voice/client"

echo "[2/3] Настройка автозапуска..."
adb $DEVICE shell "cat << 'EOF' > /data/yandex-station-chatgpt-voice/run_daemon.sh
#!/bin/sh
export LD_LIBRARY_PATH=/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib
exec /data/yandex-station-chatgpt-voice/client supervise --log=/tmp/yandex-station-chatgpt-voice.log
EOF
chmod 755 /data/yandex-station-chatgpt-voice/run_daemon.sh"

adb $DEVICE shell "cat << 'EOF' > /data/yandex-station-chatgpt-voice/start.sh
#!/bin/sh
app=/data/yandex-station-chatgpt-voice
runtime=/tmp/yandex-station-chatgpt-voice
pidfile=\"\$runtime/client.pid\"

mkdir -p \"\$app\" \"\$runtime\"
chmod 700 \"\$app\" \"\$runtime\"

[ -f \"\$app/client\" ] && [ -x \"\$app/client\" ] || exit 0
[ ! -f \"\$app/stopped\" ] || exit 0

if pidof client >/dev/null 2>&1; then
    exit 0
fi

start-stop-daemon -S -b -m -p \"\$pidfile\" -x \"\$app/run_daemon.sh\"
EOF
chmod 755 /data/yandex-station-chatgpt-voice/start.sh"

echo "[3/3] Запуск службы..."
adb $DEVICE shell "rm -f /tmp/yandex-station-chatgpt-voice/client.pid /data/yandex-station-chatgpt-voice/stopped 2>/dev/null || true"
adb $DEVICE shell "sh /data/yandex-station-chatgpt-voice/start.sh"
sleep 2
adb $DEVICE shell "pidof client >/dev/null && echo '[OK] Служба успешно запущена!' || echo '[!] Служба не запустилась'"
