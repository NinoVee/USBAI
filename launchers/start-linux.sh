#!/bin/sh
# Private AI launcher for Linux. Run: sh start-linux.sh
cd "$(dirname "$0")" || exit 1
case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  *) ARCH=x64 ;;
esac
DIR="bin/linux-$ARCH"
if [ ! -f "$DIR/privateai" ]; then
  echo "Private AI is not installed for Linux ($ARCH) on this drive."
  exit 1
fi
# Some desktops mount removable drives "noexec"; programs cannot run from
# the drive until it is remounted.
if ! [ -x "$DIR/privateai" ] && ! chmod +x "$DIR/privateai" 2>/dev/null; then
  echo "This drive is mounted without permission to run programs (noexec)."
  echo "Remount it with exec, e.g.: sudo mount -o remount,exec \"$(df --output=target . | tail -1)\""
  exit 1
fi
exec "$DIR/privateai" "$@"
