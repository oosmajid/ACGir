# ACGir

یک ابزار وب محلی و سبک برای گرفتن صدای ضبط‌های Adobe Connect و تحویل فایل MP3. می‌توانید یک لینک یا چند لینک جدا را با هم بدهید و برای هرکدام یک MP3 جداگانه بگیرید.

## اجرا (با یک کلیک)

از پوشه `dist`:

- **macOS:** روی `Start-ACGir-macOS.command` دوبار کلیک کنید (به‌طور خودکار نسخه Apple Silicon یا Intel را اجرا می‌کند).
- **Windows:** روی `Start-ACGir-Windows.bat` (یا مستقیماً `ACGir-windows-amd64.exe`) دوبار کلیک کنید.

برنامه یک وب‌سرور محلی روی `127.0.0.1` باز می‌کند و صفحه را به‌طور خودکار در مرورگر پیش‌فرض نشان می‌دهد. برای بستن برنامه، همان پنجره‌ای را که باز شده ببندید.

اگر می‌خواهید فایل اجرایی را مستقیم اجرا کنید، فایل مناسب سیستم‌عامل را از `dist` انتخاب کنید:

- macOS Apple Silicon: `ACGir-macos-arm64`
- macOS Intel: `ACGir-macos-amd64`
- Windows 64-bit: `ACGir-windows-amd64.exe`

> در macOS اگر فایل را از اینترنت گرفته‌اید و Gatekeeper اجرا را بست، یک‌بار این دستور را اجرا کنید: `xattr -dr com.apple.quarantine dist/` (لانچر `.command` این کار را خودش هم انجام می‌دهد).

## استفاده: تک‌لینک یا چند لینک

در کادر «لینک‌ها» هر خط یک لینک بگذارید:

- یک خط = یک ضبط = یک MP3.
- چند خط = چند ضبط جدا؛ برای هر لینک یک کارت با نوار پیشرفت و دکمهٔ دریافت جداگانه ساخته می‌شود.
- وقتی بیش از یک فایل آماده شد، دکمهٔ «دریافت همه (zip)» همهٔ MP3ها را در یک فایل zip می‌دهد.

برای پرهیز از فشار روی سرور، هم‌زمان حداکثر سه لینک پردازش می‌شود و بقیه در صف می‌مانند. مقدار Cookie (در صورت نیاز) برای همهٔ لینک‌ها به‌کار می‌رود.

لینک‌های واسط Moodle مثل `mod/adobeconnect/joinrecording.php?...` هم پشتیبانی می‌شوند. برنامه ابتدا query همان لینک را حفظ می‌کند و مسیر `output/lecture.zip` را امتحان می‌کند، سپس اگر لازم شد redirect یا لینک واقعی ضبط را از HTML صفحه پیدا می‌کند.

## ضبط‌های خصوصی (کوکی ورود)

اگر ضبط بدون ورود باز نمی‌شود (برنامه پیام می‌دهد لینک به «صفحهٔ ورود» رسید)، بخش **«ضبط خصوصیه و باز نمی‌شه؟»** را در خود برنامه باز کنید؛ یک راهنمای قدم‌به‌قدم و ساده برای پیدا کردن «کوکی ورود» از مرورگر در همان‌جا نوشته شده است (وارد شدن به سامانه، باز کردن DevTools با F12، تب Network، و کپی کردن خط `Cookie`). مقدار کپی‌شده را در همان کادر بچسبانید.

این کوکی فقط روی همین کامپیوتر و برای همین دانلود استفاده می‌شود؛ جایی ذخیره یا ارسال نمی‌شود. برنامه هیچ ورود یا دور زدن دسترسی‌ای انجام نمی‌دهد و فقط از نشستی استفاده می‌کند که خودتان مجاز به استفاده از آن هستید. (مقدار کوکی برای همهٔ لینک‌های وارد‌شده به‌کار می‌رود.)

## محدودیت مهم

این نسخه هیچ dependency خارجی مثل FFmpeg ندارد. بنابراین فقط این حالت‌ها را به MP3 تبدیل می‌کند:

- فایل MP3 مستقیم داخل خروجی Adobe Connect
- صدای MP3 داخل فایل‌های FLV مثل `cameraVoip*.flv`

اگر `ffmpeg` کنار فایل اجرایی برنامه یا روی PATH سیستم موجود باشد، برنامه مثل نسخه قدیمی `ACcrawler` بزرگ‌ترین فایل `.flv` یا `.mp4` داخل `lecture.zip` را با FFmpeg به MP3 تبدیل می‌کند. اگر FFmpeg موجود نباشد و سرور صدا را با AAC، Speex یا Nellymoser ذخیره کرده باشد، برنامه خطای واضح می‌دهد؛ تبدیل آن کدک‌ها به MP3 بدون encoder/decoder خارجی ممکن نیست.

## توسعه

کد با Go و فقط با standard library نوشته شده است.

برای ساخت همهٔ نسخه‌ها (مک Apple Silicon و Intel، ویندوز) همراه با لانچرهای یک‌کلیکی در `dist`:

```bash
./build.sh
```

یا دستی:

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
