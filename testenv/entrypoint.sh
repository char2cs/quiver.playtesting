#!/bin/sh
set -eu

RES="${RESOLUTION:-1280x800}"
PASS="${VNC_PASSWORD:-secret12}"
PIDS=""

if [ "${#PASS}" -gt 8 ]; then
  echo "VNC_PASSWORD must be at most 8 characters" >&2
  exit 1
fi

cleanup() { kill $PIDS 2>/dev/null || true; }
trap cleanup INT TERM EXIT

rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
Xvfb :0 -screen 0 "${RES}x24" -nolisten tcp &
PIDS="$!"

i=0
until [ -e /tmp/.X11-unix/X0 ]; do
  i=$((i + 1)); [ "$i" -gt 50 ] && { echo "Xvfb did not start" >&2; exit 1; }
  sleep 0.1
done

mkdir -p "$HOME/.vnc"
x11vnc -storepasswd "$PASS" "$HOME/.vnc/passwd" >/dev/null 2>&1

openbox-session &
PIDS="$PIDS $!"
tint2 >/dev/null 2>&1 &
PIDS="$PIDS $!"
xterm -geometry 100x30+40+40 &
PIDS="$PIDS $!"

x11vnc -display :0 -rfbport 5900 -rfbauth "$HOME/.vnc/passwd" \
  -forever -shared -noxdamage -quiet &
VNC=$!
PIDS="$PIDS $VNC"

wait "$VNC"
