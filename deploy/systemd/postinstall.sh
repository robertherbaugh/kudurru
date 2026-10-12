#!/bin/sh
# Reloads systemd after install or upgrade; never starts or enables the
# service on its own (the operator sets the encryption key first).
set -e
chown root:kudurru /etc/kudurru /etc/kudurru/kudurru.yaml /etc/kudurru/kudurru.env 2>/dev/null || true
chmod 0750 /etc/kudurru 2>/dev/null || true
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	if systemctl is-active --quiet kudurru; then
		systemctl try-restart kudurru || true
	fi
fi
