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
# Windows: single self-contained executable. Double-clicking it opens a console
# window (keep it open to keep the app running, close it to stop) and opens the
# browser on its own — no separate launcher needed.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/ACGir-Windows.exe .

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

echo "==> packaging"
# Clean up artifacts from older builds.
rm -rf dist/_mac dist/ACGir-macOS.zip dist/ACGir-Windows.zip \
       dist/ACGir-windows-amd64.exe dist/Start-ACGir-Windows.bat

# macOS bundle: both binaries (Intel + Apple Silicon) + double-click launcher,
# because a single macOS file can't cover both architectures.
mkdir -p dist/_mac
cp dist/ACGir-macos-arm64 dist/ACGir-macos-amd64 dist/Start-ACGir-macOS.command dist/_mac/
( cd dist/_mac && zip -q -r ../ACGir-macOS.zip . )
rm -rf dist/_mac
# Windows ships as a single self-contained ACGir-Windows.exe (no zip needed).

echo "==> done"
ls -la dist/
