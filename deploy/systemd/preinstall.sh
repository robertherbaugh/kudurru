#!/bin/sh
# Creates the kudurru system user and group (deb, rpm and apk packages).
set -e
if ! getent group kudurru >/dev/null 2>&1; then
	if command -v groupadd >/dev/null 2>&1; then
		groupadd --system kudurru
	else
		addgroup -S kudurru
	fi
fi
if ! getent passwd kudurru >/dev/null 2>&1; then
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --gid kudurru --home-dir /var/lib/kudurru --no-create-home --shell /usr/sbin/nologin kudurru
	else
		adduser -S -D -H -G kudurru -h /var/lib/kudurru -s /sbin/nologin kudurru
	fi
fi
