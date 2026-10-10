# Single instance with docker compose

```sh
mkdir -p secrets
openssl rand -base64 32 > secrets/encryption_key
echo 'change-me' > secrets/admin_password
docker compose up -d
dig @127.0.0.1 example.com
curl -s localhost:8081/healthz
```

Add `--profile edge` to put dnsdist in front (DoT/DoH/DoQ and a packet cache); the
edge image and `kudurru edge run` arrive in milestone M4.
