#!/bin/sh
# Builds a complete Private AI drive image in dist/PRIVATE-AI.
#
#   scripts/build-drive.sh            # binaries + launchers + config only
#   scripts/build-drive.sh --fetch    # also download runtimes and models
#
# Then copy the contents of dist/PRIVATE-AI to the root of an exFAT-formatted
# USB drive (exFAT is readable and writable on Windows, macOS and Linux).
set -eu
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
  echo "Go is not installed. Install Go 1.24 or newer, then run this again:"
  echo "  macOS:   brew install go   (or the installer from https://go.dev/dl/)"
  echo "  Windows: winget install GoLang.Go   (or https://go.dev/dl/)"
  echo "  Linux:   https://go.dev/doc/install"
  exit 1
fi
OUT=dist/PRIVATE-AI
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

mkdir -p "$OUT/bin" "$OUT/runtime" "$OUT/models/chat" "$OUT/models/embed" "$OUT/models/specialist" "$OUT/data"

for target in windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os=${target%/*}; arch=${target#*/}
  dos=$os; [ "$os" = darwin ] && dos=macos
  darch=$arch; [ "$arch" = amd64 ] && darch=x64
  ext=""; [ "$os" = windows ] && ext=.exe
  echo "building bin/$dos-$darch"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$OUT/bin/$dos-$darch/privateai$ext" ./cmd/privateai
done

[ -f "$OUT/config.json" ] || cp drive/config.json "$OUT/config.json"
cp drive/README.txt "$OUT/README.txt"
cp launchers/Start-Windows.bat "$OUT/Start-Windows.bat"
cp launchers/Start-macOS.command "$OUT/Start-macOS.command"
cp launchers/start-linux.sh "$OUT/start-linux.sh"
chmod +x "$OUT/Start-macOS.command" "$OUT/start-linux.sh"

if [ "${1:-}" = "--fetch" ]; then
  go run ./cmd/drivetool fetch-runtime -drive "$OUT"
  go run ./cmd/drivetool fetch-models -drive "$OUT"
fi
go run ./cmd/drivetool check -drive "$OUT"
