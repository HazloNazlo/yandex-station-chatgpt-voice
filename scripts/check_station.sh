#!/usr/bin/env bash
set -euo pipefail

echo "========================================================="
echo "  Проверка подключения к Яндекс Станции (ADB)"
echo "========================================================="
echo

command -v adb >/dev/null 2>&1 || {
    echo "[ОШИБКА] adb не найден. Установите adb (apt install adb / brew install android-platform-tools)"
    exit 1
}

echo "Поиск подключенных устройств..."
adb devices
echo

if adb get-state >/dev/null 2>&1; then
    echo "[OK] Устройство найдено! Проверка прав root..."
    adb shell "id"
    echo
    adb shell "echo Модель: \$(cat /etc/yandex/model 2>/dev/null || cat /system/build.prop | grep ro.product.model | cut -d= -f2); echo IP: \$(ip route get 1.1.1.1 2>/dev/null | awk '{print \$7}')"
    echo
    echo "========================================================="
    echo "  Колонка готова к работе!"
    echo "========================================================="
else
    echo "[!] Колонка не найдена по USB."
    echo "Подключитесь по Wi-Fi: adb connect IP_СТАНЦИИ:5555"
fi
