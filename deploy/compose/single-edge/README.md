# Single instance behind dnsdist

dnsdist (GPL-2.0, a separate program in the `kudurru-dnsdist` image, see
`images/dnsdist/`) owns ports 53, 853 and 443, terminates DoT, DoH, DoQ and
DoH3 with `certs/tls.crt` and `certs/tls.key`, and forwards to kudurru on
`127.0.0.1:5353` with PROXYv2 so kudurru logs the real client address.

```sh
mkdir -p secrets certs
openssl rand -base64 32 > secrets/encryption_key
openssl rand -base64 18 > secrets/admin_password
cp /path/to/fullchain.pem certs/tls.crt
cp /path/to/privkey.pem certs/tls.key
docker compose up -d
kdig @127.0.0.1 +tls example.com
```

v0.1.0 includes the supervisor image and `kudurru edge run`. Set
`KUDURRU_LICENSE_KEY` to a key with the edge entitlement before starting
Compose (a free eligible personal key covers it). See
[docs/deploy/docker.md](../../../docs/deploy/docker.md).
