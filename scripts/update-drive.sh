#!/bin/sh
# Updates an existing Private AI drive in place: new program files, launchers,
# and model entries added by updates (such as personalities). Models, your
# vault, chats and settings are left as they are.
#
#   scripts/update-drive.sh "/Volumes/GANG AI"
#
# Shut Private AI down first (Settings → Shut down).
set -eu
if [ $# -ne 1 ]; then
  echo "usage: scripts/update-drive.sh /path/to/drive   (for example \"/Volumes/GANG AI\")"
  exit 1
fi
if [ ! -f "$1/config.json" ]; then
  echo "$1 is not a Private AI drive (no config.json there). Is the drive plugged in?"
  [ -d /Volumes ] && { echo "Drives on this Mac:"; ls /Volumes; }
  exit 1
fi
DRIVE=$(cd "$1" && pwd)
cd "$(dirname "$0")/.."

scripts/build-drive.sh
echo "updating $DRIVE"
cp -R dist/PRIVATE-AI/bin "$DRIVE/"
cp dist/PRIVATE-AI/README.txt dist/PRIVATE-AI/Start-Windows.bat dist/PRIVATE-AI/Start-macOS.command dist/PRIVATE-AI/start-linux.sh "$DRIVE/"
go run ./cmd/drivetool sync-config -drive "$DRIVE" -template drive/config.json
go run ./cmd/drivetool check -drive "$DRIVE"
echo
echo "Drive updated. Start Private AI from the drive."
