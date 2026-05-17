@echo off
setlocal EnableExtensions
chcp 65001 >nul

cd /d "%~dp0"

echo این فایل فقط برای ساخت نسخه قابل حمل ویندوز است.
echo خروجی نهایی در dist\ACcrawler ساخته می‌شود و برای اجرا به Python، venv یا ffmpeg نیاز ندارد.
echo.

set "PY_CMD="
where py >nul 2>nul
if not errorlevel 1 set "PY_CMD=py -3"

if not defined PY_CMD (
    where python >nul 2>nul
    if not errorlevel 1 set "PY_CMD=python"
)

if not defined PY_CMD (
    echo Python برای ساخت نسخه exe پیدا نشد.
    echo برای ساخت فقط یک‌بار Python 3.11 یا جدیدتر را روی همین سیستم نصب کنید.
    echo کاربر نهایی به Python نیاز نخواهد داشت.
    pause
    exit /b 1
)

set "BUILD_VENV=.build-venv"
set "BUILD_PYTHON=%CD%\%BUILD_VENV%\Scripts\python.exe"

if exist "%BUILD_VENV%\Lib\site-packages\gradio" (
    echo پاکسازی محیط build قدیمی و سنگین...
    rmdir /s /q "%BUILD_VENV%"
)

if not exist "%BUILD_PYTHON%" (
    echo ساخت محیط موقت build...
    %PY_CMD% -m venv "%BUILD_VENV%"
    if errorlevel 1 (
        echo ساخت محیط موقت ناموفق بود.
        pause
        exit /b 1
    )
)

if not exist "%BUILD_PYTHON%" (
    echo فایل Python داخل محیط build پیدا نشد:
    echo "%BUILD_PYTHON%"
    pause
    exit /b 1
)

echo نصب وابستگی‌های ساخت...
"%BUILD_PYTHON%" -m pip install --upgrade pip
if errorlevel 1 (
    echo به‌روزرسانی pip ناموفق بود.
    pause
    exit /b 1
)

"%BUILD_PYTHON%" -m pip install -r requirements-build.txt
if errorlevel 1 (
    echo نصب وابستگی‌ها ناموفق بود.
    pause
    exit /b 1
)

echo ساخت برنامه قابل حمل...
"%BUILD_PYTHON%" -m PyInstaller --clean --noconfirm ACcrawler.spec
if errorlevel 1 (
    echo ساخت برنامه ناموفق بود.
    pause
    exit /b 1
)

echo.
echo انجام شد.
echo فایل اجرای نهایی:
echo dist\ACcrawler\ACcrawler.exe
echo.
where powershell >nul 2>nul
if not errorlevel 1 (
    echo ساخت فایل zip پرتابل...
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Compress-Archive -Path 'dist\ACcrawler' -DestinationPath 'dist\ACcrawler-portable.zip' -Force"
    if not errorlevel 1 (
        echo فایل zip آماده:
        echo dist\ACcrawler-portable.zip
        echo.
    )
)

echo برای انتقال به سیستم دیگر، فایل dist\ACcrawler-portable.zip یا کل پوشه dist\ACcrawler را کپی کنید.
pause
