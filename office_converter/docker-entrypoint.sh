#!/usr/bin/env bash
set -euo pipefail

export XDG_RUNTIME_DIR=/tmp/runtime-root
mkdir -p "${XDG_RUNTIME_DIR}"
chmod 700 "${XDG_RUNTIME_DIR}"

mkdir -p /root/.config/Kingsoft

if ! pgrep -f "Xorg ${DISPLAY}" >/dev/null 2>&1; then
  Xorg "${DISPLAY}" -config /etc/X11/dummy.conf >/tmp/xorg.log 2>&1 &
fi

if ! pgrep -f "Xorg ${DISPLAY}" >/dev/null 2>&1 && ! pgrep -f "Xvfb ${DISPLAY}" >/dev/null 2>&1; then
  Xvfb "${DISPLAY}" -screen 0 1280x1024x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
fi

if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
  dbus-launch --sh-syntax > /tmp/dbus.env
  # shellcheck disable=SC1091
  . /tmp/dbus.env
fi

# Optional: mount a pre-accepted Office.conf to bypass first-run EULA dialog.
if [ -f /app/Office.conf ] && [ ! -f /root/.config/Kingsoft/Office.conf ]; then
  cp /app/Office.conf /root/.config/Kingsoft/Office.conf
fi

if [ -d /usr/local/share/fonts/custom ]; then
  fc-cache -f /usr/local/share/fonts/custom >/tmp/fc-cache-custom.log 2>&1 || true
fi

exec "$@"
