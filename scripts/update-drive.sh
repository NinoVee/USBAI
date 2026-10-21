#!/bin/sh
# Updates an existing Private AI drive in place: new program files, launchers,
# and model entries added by updates (such as personalities). Models, your
# vault, chats and settings are left as they are.
#
#   scripts/update-drive.sh "/Volumes/GANG AI"          # keeps the drive's edition
#   scripts/update-drive.sh --pro "/Volumes/GANG PRO"   # makes it a Pro drive
#
# A standard drive stays standard and a Pro drive stays Pro: the edition is
# read from the drive's own config.json.
#
# Shut Private AI down first (Settings → Shut down).
set -eu
PRO=""
if [ "${1:-}" = "--pro" ]; then PRO=1; shift; fi
if [ $# -ne 1 ]; then
  echo "usage: scripts/update-drive.sh [--pro] /path/to/drive   (for example \"/Volumes/GANG AI\")"
  exit 1
fi
if [ ! -f "$1/config.json" ]; then
  echo "$1 is not a Private AI drive (no config.json there). Is the drive plugged in?"
  [ -d /Volumes ] && { echo "Drives on this Mac:"; ls /Volumes; }
  exit 1
fi
DRIVE=$(cd "$1" && pwd)
cd "$(dirname "$0")/.."
if grep -q '"edition": *"pro"' "$DRIVE/config.json"; then PRO=1; fi

if [ -n "$PRO" ]; then
  echo "This is a Pro drive."
  scripts/build-drive.sh --pro
  OUT=dist/GANG-AI-PRO
  TEMPLATE=drive/config.json,drive/pro.json
else
  scripts/build-drive.sh
  OUT=dist/PRIVATE-AI
  TEMPLATE=drive/config.json
fi
echo "updating $DRIVE"
cp -R "$OUT/bin" "$DRIVE/"
cp "$OUT/README.txt" "$OUT/Start-Windows.bat" "$OUT/Start-macOS.command" "$OUT/start-linux.sh" "$DRIVE/"
go run ./cmd/drivetool sync-config -drive "$DRIVE" -template "$TEMPLATE"
go run ./cmd/drivetool check -drive "$DRIVE"
echo
echo "Drive updated. Start Private AI from the drive."
