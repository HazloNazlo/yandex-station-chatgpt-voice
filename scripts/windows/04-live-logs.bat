@echo off
chcp 65001 >nul
title Логи Яндекс Станции - ChatGPT Live
echo =========================================================
echo   Просмотр логов в реальном времени (Ctrl+C для выхода)
echo =========================================================
echo.

where.exe adb >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    set "ADB=adb"
    goto stream
)

if exist "%~dp0..\..\platform-tools\adb.exe" (
    set "ADB=%~dp0..\..\platform-tools\adb.exe"
    goto stream
)

echo [ОШИБКА] ADB не найден.
pause
exit /b 1

:stream
set "KEY=%LOCALAPPDATA%\StationMiniRoot\work\keys\adbkey"
if exist "%KEY%" (
    set "ADB_VENDOR_KEYS=%KEY%"
)

"%ADB%" shell "tail -f /tmp/yandex-station-chatgpt-voice.log"
pause
