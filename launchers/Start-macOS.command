#!/bin/sh
# Private AI launcher for macOS. Double-click to start.
cd "$(dirname "$0")" || exit 1
case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  *) ARCH=x64 ;;
esac
DIR="bin/macos-$ARCH"
if [ ! -x "$DIR/privateai" ] && [ ! -f "$DIR/privateai" ]; then
  echo "Private AI is not installed for macOS ($ARCH) on this drive."
  read -r _; exit 1
fi
# Files copied from the internet are quarantined by Gatekeeper; clear that
# flag for this drive's own binaries so the runtime can start.
xattr -dr com.apple.quarantine "bin/macos-$ARCH" "runtime/macos-$ARCH" 2>/dev/null
chmod +x "$DIR/privateai" "runtime/macos-$ARCH/llama-server" 2>/dev/null
exec "$DIR/privateai" "$@"
