@echo off
chcp 65001 >nul
title Авторизация ChatGPT Voice для Яндекс Станции
echo =========================================================
echo   Вход в аккаунт ChatGPT (Web Voice)
echo =========================================================
echo.

where.exe py >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    py -u "%~dp0login_helper.py"
    pause
    exit /b
)

where.exe python >nul 2>&1
if %ERRORLEVEL% EQU 0 (
    python -u "%~dp0login_helper.py"
    pause
    exit /b
)

echo [ОШИБКА] Python не найден в системе.
echo Установите Python 3 (https://www.python.org/) и отметьте галочку "Add to PATH".
pause
