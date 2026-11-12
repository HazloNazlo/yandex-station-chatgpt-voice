@echo off
chcp 65001 >nul
title Установка / Обновление GPT Live на Яндекс Станцию
echo =========================================================
echo   Установка GPT Live (+5dB громкость + Интернет-поиск)
echo =========================================================
echo.

set "SCRIPT_DIR=%~dp0"
set "REPO_ROOT=%SCRIPT_DIR%..\..\"
set "BIN=%REPO_ROOT%build\yandex-station-chatgpt-voice"

where.exe adb >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    set "ADB=adb"
    goto check_bin
)

if exist "%REPO_ROOT%platform-tools\adb.exe" (
    set "ADB=%REPO_ROOT%platform-tools\adb.exe"
    goto check_bin
)

if exist "%REPO_ROOT%..\platform-tools\adb.exe" (
    set "ADB=%REPO_ROOT%..\platform-tools\adb.exe"
    goto check_bin
)

echo [ОШИБКА] ADB не найден.
pause
exit /b 1

:check_bin
if not exist "%BIN%" (
    echo [ОШИБКА] Скомпилированный бинарник не найден: %BIN%
    echo Скомпилируйте его через WSL / Linux командой: tools/build-arm.sh build
    echo Или скачайте готовый бинарник из раздела Releases на GitHub.
    pause
    exit /b 1
)

set "KEY=%LOCALAPPDATA%\StationMiniRoot\work\keys\adbkey"
if exist "%KEY%" (
    set "ADB_VENDOR_KEYS=%KEY%"
)

echo [1/3] Поиск подключенной Станции...
"%ADB%" get-state >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    echo       Найдено USB-подключение!
    set "DEVICE="
    goto do_deploy
)

echo       Устройство не найдено по USB. Введите IP Станции:
set /p STATION_IP="IP адрес колонки (например, 192.168.1.100): "
if "%STATION_IP%"=="" (
    echo [ОШИБКА] IP не указан.
    pause
    exit /b 1
)

"%ADB%" connect %STATION_IP%:5555
timeout /t 2 >nul
"%ADB%" -s %STATION_IP%:5555 get-state >nul 2>&1
if %ERRORLEVEL% NEQ 0 (
    echo [ОШИБКА] Не удалось подключиться к %STATION_IP%:5555
    pause
    exit /b 1
)
set "DEVICE=-s %STATION_IP%:5555"

:do_deploy
echo.
echo [2/3] Загрузка бинарника на колонку...
"%ADB%" %DEVICE% shell "mkdir -p /data/yandex-station-chatgpt-voice"
"%ADB%" %DEVICE% shell "killall client 2>/dev/null; cp -f /data/yandex-station-chatgpt-voice/client /data/yandex-station-chatgpt-voice/client.previous 2>/dev/null; true"
"%ADB%" %DEVICE% push "%BIN%" /data/yandex-station-chatgpt-voice/client
if %ERRORLEVEL% NEQ 0 (
    echo [ОШИБКА] Не удалось отправить файл через ADB!
    pause
    exit /b 1
)
"%ADB%" %DEVICE% shell "chmod 755 /data/yandex-station-chatgpt-voice/client"

echo.
echo [3/3] Настройка автозапуска и запуск службы...
"%ADB%" %DEVICE% shell "cat << 'EOF' > /data/yandex-station-chatgpt-voice/run_daemon.sh
#!/bin/sh
export LD_LIBRARY_PATH=/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib
exec /data/yandex-station-chatgpt-voice/client supervise --log=/tmp/yandex-station-chatgpt-voice.log
EOF
chmod 755 /data/yandex-station-chatgpt-voice/run_daemon.sh"

"%ADB%" %DEVICE% shell "cat << 'EOF' > /data/yandex-station-chatgpt-voice/start.sh
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

"%ADB%" %DEVICE% shell "rm -f /tmp/yandex-station-chatgpt-voice/client.pid /data/yandex-station-chatgpt-voice/stopped 2>/dev/null; true"
"%ADB%" %DEVICE% shell "sh /data/yandex-station-chatgpt-voice/start.sh"
timeout /t 2 >nul
"%ADB%" %DEVICE% shell "pidof client >/dev/null && echo [OK] Служба client успешно запущена! || echo [!] Служба не запустилась"

echo.
echo =========================================================
echo   [УСПЕХ] Клиент установлен и запущен!
echo =========================================================
echo.
pause
