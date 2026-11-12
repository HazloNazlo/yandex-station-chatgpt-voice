@echo off
chcp 65001 >nul
title Перезапуск службы ChatGPT Live на Станции
echo =========================================================
echo   Перезапуск службы на Яндекс Станции
echo =========================================================
echo.

where.exe adb >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    set "ADB=adb"
    goto restart
)

if exist "%~dp0..\..\platform-tools\adb.exe" (
    set "ADB=%~dp0..\..\platform-tools\adb.exe"
    goto restart
)

echo [ОШИБКА] ADB не найден.
pause
exit /b 1

:restart
set "KEY=%LOCALAPPDATA%\StationMiniRoot\work\keys\adbkey"
if exist "%KEY%" (
    set "ADB_VENDOR_KEYS=%KEY%"
)

echo Остановка старых процессов...
"%ADB%" shell "killall client 2>/dev/null; rm -f /tmp/yandex-station-chatgpt-voice/client.pid /data/yandex-station-chatgpt-voice/stopped; sleep 1"
echo Запуск службы...
"%ADB%" shell "sh /data/yandex-station-chatgpt-voice/start.sh"
timeout /t 2 >nul
"%ADB%" shell "pidof client >/dev/null && echo [OK] Служба успешно перезапущена! || echo [!] Служба не запущена"
echo.
pause
