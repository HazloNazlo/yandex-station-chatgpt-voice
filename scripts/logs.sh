#!/usr/bin/env bash
echo "Подключение к логам Яндекс Станции (Ctrl+C для выхода)..."
adb shell "tail -f /tmp/yandex-station-chatgpt-voice.log"
