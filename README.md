# ACGir

یک ابزار وب محلی و سبک برای گرفتن صدای ضبط‌های Adobe Connect و تحویل فایل MP3.

## اجرا

از پوشه `dist` فایل مناسب سیستم‌عامل را اجرا کنید:

- macOS Apple Silicon: `ACGir-macos-arm64`
- macOS Intel: `ACGir-macos-amd64`
- Windows 64-bit: `ACGir-windows-amd64.exe`

برنامه یک وب‌سرور محلی روی `127.0.0.1` باز می‌کند و صفحه را در مرورگر پیش‌فرض نشان می‌دهد. لینک ضبط Adobe Connect را وارد کنید و بعد از پایان پردازش، فایل MP3 را از همان صفحه دریافت کنید.

لینک‌های واسط Moodle مثل `mod/adobeconnect/joinrecording.php?...` هم پشتیبانی می‌شوند. برنامه ابتدا query همان لینک را حفظ می‌کند و مسیر `output/lecture.zip` را امتحان می‌کند، سپس اگر لازم شد redirect یا لینک واقعی ضبط را از HTML صفحه پیدا می‌کند.

## ضبط‌های خصوصی

اگر لینک ضبط بدون ورود قابل دریافت نیست، مقدار Cookie نشست Adobe Connect را در بخش «نشست خصوصی / Cookie» وارد کنید. برنامه ورود یا دور زدن دسترسی انجام نمی‌دهد؛ فقط از نشستی استفاده می‌کند که خودتان مجاز به استفاده از آن هستید.

اگر لاگ برنامه نشان داد لینک به `login/index.php` برگشته، یعنی سامانه Moodle نشست معتبر ندیده است. در این حالت باید در مرورگر وارد همان سامانه شوید و Cookie نشست را وارد کنید، یا لینک مستقیم ضبط/zip را از صفحه‌ای که بعد از ورود باز می‌شود به برنامه بدهید.

## محدودیت مهم

این نسخه هیچ dependency خارجی مثل FFmpeg ندارد. بنابراین فقط این حالت‌ها را به MP3 تبدیل می‌کند:

- فایل MP3 مستقیم داخل خروجی Adobe Connect
- صدای MP3 داخل فایل‌های FLV مثل `cameraVoip*.flv`

اگر `ffmpeg` کنار فایل اجرایی برنامه یا روی PATH سیستم موجود باشد، برنامه مثل نسخه قدیمی `ACcrawler` بزرگ‌ترین فایل `.flv` یا `.mp4` داخل `lecture.zip` را با FFmpeg به MP3 تبدیل می‌کند. اگر FFmpeg موجود نباشد و سرور صدا را با AAC، Speex یا Nellymoser ذخیره کرده باشد، برنامه خطای واضح می‌دهد؛ تبدیل آن کدک‌ها به MP3 بدون encoder/decoder خارجی ممکن نیست.

## توسعه

کد با Go و فقط با standard library نوشته شده است.

```bash
go test ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-macos-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-macos-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-windows-amd64.exe .
```

## منابع رفتار Adobe Connect

- Adobe: پیدا کردن ضبط با `mode=xml`: https://helpx.adobe.com/adobe-connect/kb/locate-connect-recording-using-connect-recording.html
- Adobe: ساخت URL دانلود zip خروجی: https://helpx.adobe.com/lv/adobe-connect/webservices/getting-started-connect-web-services.html
- Adobe: مسیرهای `output/indexstream.xml` و `output/mainstream.xml`: https://helpx.adobe.com/adobe-connect/kb/troubleshoot-recording-issues.html
