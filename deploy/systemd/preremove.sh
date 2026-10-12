#!/bin/sh
# Stops the service before the package is removed (not on upgrade).
set -e
case "$1" in
	upgrade|1) exit 0 ;; # deb passes "upgrade", rpm passes the remaining package count
esac
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl disable --now kudurru >/dev/null 2>&1 || true
fi
