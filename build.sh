#!/bin/bash
# Build ACGir binaries for macOS (Apple Silicon + Intel) and Windows,
# and write one-click launchers into dist/.
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p dist

echo "==> go test"
go test ./...

echo "==> building binaries"
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-macos-arm64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-macos-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-windows-amd64.exe .

echo "==> writing launchers"

# macOS: double-click opens Terminal, picks the right binary, opens the browser.
cat > "dist/Start-ACGir-macOS.command" <<'LAUNCHER'
#!/bin/bash
cd "$(dirname "$0")" || exit 1
if [ "$(uname -m)" = "arm64" ]; then
  BIN="./ACGir-macos-arm64"
else
  BIN="./ACGir-macos-amd64"
fi
chmod +x "$BIN" 2>/dev/null || true
# Remove the "downloaded from the internet" quarantine so Gatekeeper allows it.
xattr -dr com.apple.quarantine "$BIN" 2>/dev/null || true
echo "ACGir در حال اجراست و در مرورگر باز می‌شود."
echo "این پنجره را باز نگه دارید؛ برای بستن برنامه پنجره را ببندید یا Ctrl+C بزنید."
echo
exec "$BIN"
LAUNCHER
chmod +x "dist/Start-ACGir-macOS.command"

# Windows: double-click runs the .exe (it opens the browser itself).
cat > "dist/Start-ACGir-Windows.bat" <<'LAUNCHER'
@echo off
cd /d "%~dp0"
echo ACGir is running and will open in your browser.
echo Keep this window open; close it (or press Ctrl+C) to stop the app.
echo.
ACGir-windows-amd64.exe
pause
LAUNCHER

echo "==> done"
ls -la dist/
