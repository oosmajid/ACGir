@echo off
setlocal EnableExtensions
chcp 65001 >nul

:: رفتن به مسیری که همین فایل در آن قرار دارد
cd /d "%~dp0"

set "PY_CMD="
where py >nul 2>nul
if not errorlevel 1 set "PY_CMD=py -3"

if not defined PY_CMD (
    where python >nul 2>nul
    if not errorlevel 1 set "PY_CMD=python"
)

if not defined PY_CMD (
    echo Python روی سیستم پیدا نشد.
    echo برای اجرای سورس باید Python نصب باشد. برای نسخه بدون پیش‌نیاز، build_windows.bat را روی ویندوز اجرا کنید و پوشه dist\ACcrawler را استفاده کنید.
    pause
    exit /b 1
)

if not exist "venv\Scripts\python.exe" (
    echo ساخت محیط مجازی...
    %PY_CMD% -m venv venv
    if errorlevel 1 (
        echo ساخت محیط مجازی ناموفق بود.
        pause
        exit /b 1
    )
)

:: فعال کردن محیط مجازی و نصب وابستگی‌ها
if exist "venv\Lib\site-packages\gradio" (
    echo پاکسازی محیط قدیمی و سنگین...
    rmdir /s /q "venv"
    %PY_CMD% -m venv venv
    if errorlevel 1 (
        echo ساخت محیط مجازی ناموفق بود.
        pause
        exit /b 1
    )
)

"venv\Scripts\python.exe" -c "import imageio_ffmpeg, requests" >nul 2>nul
if errorlevel 1 (
    "venv\Scripts\python.exe" -m pip install -r requirements.txt
    if errorlevel 1 (
        echo نصب وابستگی‌ها ناموفق بود.
        pause
        exit /b 1
    )
)

:: اجرای برنامه
"venv\Scripts\python.exe" main.py

pause
