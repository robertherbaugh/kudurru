#!/bin/bash
# Keep the shared machine from filling up: trim stale Go build cache entries
# (safe: the cache rebuilds on demand) when free space runs low. A trim can
# make a build in flight fail once ("could not import ... no such file"); a
# full disk is worse (it reached 100% on 2026-11-13 with a 120-minute rule).
while true; do
  free=$(df --output=avail -BG / | tail -1 | tr -dc 0-9)
  if [ "$free" -lt 8 ]; then
    find /home/vercel-sandbox/.cache/go-build -type f -mmin +75 -delete 2>/dev/null
    find /tmp -maxdepth 1 \( -name 'go-build*' -o -name 'kudurru-e2e-*' \) -mmin +180 -exec rm -rf {} + 2>/dev/null
    free2=$(df --output=avail -BG / | tail -1 | tr -dc 0-9)
    echo "$(date -u +%FT%TZ) free ${free}G -> ${free2}G after trimming entries older than 75m"
    if [ "$free2" -lt 5 ]; then
      find /home/vercel-sandbox/.cache/go-build -type f -mmin +40 -delete 2>/dev/null
      echo "$(date -u +%FT%TZ) free $(df --output=avail -BG / | tail -1 | tr -dc 0-9)G after trimming entries older than 40m"
    fi
  fi
  sleep 60
done
