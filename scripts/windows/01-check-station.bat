@echo off
chcp 65001 >nul
title Проверка подключения к Яндекс Станции
echo =========================================================
echo   Проверка подключения к Яндекс Станции (ADB)
echo =========================================================
echo.

where.exe adb >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    set "ADB=adb"
    goto check
)

if exist "%~dp0..\..\platform-tools\adb.exe" (
    set "ADB=%~dp0..\..\platform-tools\adb.exe"
    goto check
)

if exist "%LOCALAPPDATA%\StationMiniRoot\work\platform-tools\adb.exe" (
    set "ADB=%LOCALAPPDATA%\StationMiniRoot\work\platform-tools\adb.exe"
    goto check
)

echo [ОШИБКА] ADB не найден в системе.
echo Установите platform-tools или запустите StationMiniRoot.
pause
exit /b 1

:check
set "KEY=%LOCALAPPDATA%\StationMiniRoot\work\keys\adbkey"
if exist "%KEY%" (
    set "ADB_VENDOR_KEYS=%KEY%"
)

echo Поиск подключенных устройств...
"%ADB%" devices
echo.

"%ADB%" get-state >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    echo [OK] Устройство найдено! Проверка прав root...
    "%ADB%" shell "id"
    echo.
    "%ADB%" shell "echo Модель: $(cat /etc/yandex/model 2>/dev/null || cat /system/build.prop | grep ro.product.model | cut -d= -f2); echo IP: $(ip route get 1.1.1.1 2>/dev/null | awk '{print $7}')"
    echo.
    echo =========================================================
    echo   Колонка готова к работе!
    echo =========================================================
) else (
    echo [!] Колонка не найдена.
    echo Убедитесь, что колонка подключена по USB кабелем передачи данных
    echo или подключитесь по Wi-Fi командой: adb connect IP_СТАНЦИИ:5555
)

echo.
pause
