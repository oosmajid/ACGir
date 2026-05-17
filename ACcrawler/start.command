#!/bin/bash
set -e

# رفتن به مسیری که همین فایل اجرایی در آن قرار دارد
cd "$(dirname "$0")"

# ساخت و فعال کردن محیط مجازی
if [ ! -x "venv/bin/python" ]; then
    python3 -m venv venv
fi

if compgen -G "venv/lib/python*/site-packages/gradio" >/dev/null; then
    echo "پاکسازی محیط قدیمی و سنگین..."
    rm -rf venv
    python3 -m venv venv
fi

venv/bin/python -c "import imageio_ffmpeg, requests" >/dev/null 2>&1 || venv/bin/python -m pip install -r requirements.txt

# اجرای برنامه
venv/bin/python main.py
