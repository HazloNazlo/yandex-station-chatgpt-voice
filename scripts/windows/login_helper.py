# -*- coding: utf-8 -*-
"""
Помощник авторизации ChatGPT Voice для Яндекс Станции (Windows).
Автоматически открывает отдельное окно Google Chrome для входа в chatgpt.com,
извлекает токен сессии (accessToken + deviceId) по протоколу CDP
и сохраняет его на Яндекс Станцию. Платный OpenAI API ключ НЕ требуется!
"""

import os
import sys
import json
import time
import shutil
import tempfile
import subprocess
from pathlib import Path

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8")
if hasattr(sys.stderr, "reconfigure"):
    sys.stderr.reconfigure(encoding="utf-8")
if hasattr(sys.stdin, "reconfigure"):
    sys.stdin.reconfigure(encoding="utf-8")

SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent.parent
TOOLS_DIR = REPO_ROOT / "tools"

sys.path.insert(0, str(TOOLS_DIR))
try:
    import browser_login
except ImportError:
    pass


def find_adb():
    found = shutil.which("adb")
    if found:
        return found
    candidates = [
        REPO_ROOT / "platform-tools" / "adb.exe",
        REPO_ROOT.parent / "platform-tools" / "adb.exe",
        Path(os.path.expandvars(r"%LOCALAPPDATA%\StationMiniRoot\work\platform-tools\adb.exe")),
    ]
    for c in candidates:
        if c.is_file():
            return str(c)
    return "adb"


def get_chrome_path():
    candidates = [
        r"C:\Program Files\Google\Chrome\Application\chrome.exe",
        r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
        os.path.expandvars(r"%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe"),
        r"C:\Program Files\Chromium\Application\chrome.exe",
        r"C:\Program Files\BraveSoftware\Brave-Browser\Application\brave.exe",
        r"C:\Program Files\Microsoft\Edge\Application\msedge.exe",
    ]
    for c in candidates:
        if os.path.isfile(c):
            return c
    return shutil.which("chrome") or shutil.which("google-chrome")


def run_adb(adb_bin, *args):
    cmd = [adb_bin] + list(args)
    res = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    return res.returncode, res.stdout.strip(), res.stderr.strip()


def main():
    print("=" * 65)
    print("      Авторизация ChatGPT Voice на Яндекс Станции      ")
    print("=" * 65)
    print()

    adb = find_adb()
    key = os.path.expandvars(r"%LOCALAPPDATA%\StationMiniRoot\work\keys\adbkey")
    if os.path.isfile(key):
        os.environ["ADB_VENDOR_KEYS"] = key

    # 1. Проверяем ADB
    print("[1/4] Проверка подключения к Станции...")
    code, out, _ = run_adb(adb, "devices")
    devices = [line for line in out.splitlines() if "\tdevice" in line]
    if not devices:
        print("[!] Колонка не найдена через ADB.")
        ip = input("Введите IP адрес колонки в локальной сети (или Enter для отмены): ").strip()
        if ip:
            run_adb(adb, "connect", f"{ip}:5555")
            time.sleep(2)
            code, out, _ = run_adb(adb, "devices")
            devices = [line for line in out.splitlines() if "\tdevice" in line]

    if not devices:
        print("[ОШИБКА] Не удалось обнаружить Яндекс Станцию.")
        print("Убедитесь, что колонка подключена по USB или доступна по Wi-Fi.")
        return 1

    device_id = devices[0].split("\t")[0]
    print(f"       Колонка подключена: {device_id}")

    # 2. Поиск браузера
    chrome = get_chrome_path()
    if not chrome:
        print("[ОШИБКА] Google Chrome или совместимый Chromium-браузер не найден.")
        return 1

    print()
    print("[2/4] Запуск чистого окна браузера...")
    print(f"       Браузер: {chrome}")

    temp_dir = tempfile.mkdtemp(prefix="chatgpt_yandex_login_")
    port = 9222
    args = [
        chrome,
        f"--remote-debugging-port={port}",
        f"--user-data-dir={temp_dir}",
        "--no-first-run",
        "--no-default-browser-check",
        "https://chatgpt.com/",
    ]

    proc = subprocess.Popen(args)
    time.sleep(2)

    print()
    print("[3/4] Ожидание авторизации в ChatGPT...")
    print("       1. В открывшемся окне нажмите 'Log in' / 'Войти'.")
    print("       2. Зайдите в свой обычный аккаунт OpenAI.")
    print("       3. После загрузки чата этот скрипт автоматически перехватит сессию.")
    print()

    session_data = None
    try:
        session_data = browser_login.capture_session("127.0.0.1", port, timeout=300)
    except Exception as e:
        print(f"[!] Ошибка ожидания сессии: {e}")
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=3)
        except Exception:
            pass
        shutil.rmtree(temp_dir, ignore_errors=True)

    if not session_data:
        print("[ОШИБКА] Не удалось получить данные сессии. Попробуйте снова.")
        return 1

    # 3. Сохраняем сессию на колонку
    print()
    print("[4/4] Загрузка сессии на Яндекс Станцию...")
    session_json_str = json.dumps(session_data, ensure_ascii=False, indent=2)

    local_session = REPO_ROOT / "session.json"
    local_session.write_text(session_json_str, encoding="utf-8")

    run_adb(adb, "-s", device_id, "shell", "mkdir -p /data/yandex-station-chatgpt-voice")
    run_adb(adb, "-s", device_id, "push", str(local_session), "/data/yandex-station-chatgpt-voice/session.json")
    run_adb(adb, "-s", device_id, "shell", "chmod 600 /data/yandex-station-chatgpt-voice/session.json")

    # Перезапускаем службу на колонке
    run_adb(adb, "-s", device_id, "shell", "killall client 2>/dev/null; sleep 1; sh /data/yandex-station-chatgpt-voice/start.sh")

    print()
    print("=" * 65)
    print("  [УСПЕХ] Авторизация завершена!")
    print("  Сессия успешно сохранена на Яндекс Станции.")
    print("  Скажите 'Яндекс' колонке, дождитесь звукового сигнала и спросите:")
    print("  'Привет! Как твои дела?' или 'Какой сейчас курс доллара?'")
    print("=" * 65)
    return 0


if __name__ == "__main__":
    sys.exit(main())
