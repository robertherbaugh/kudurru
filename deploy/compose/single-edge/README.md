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

The supervisor image and `kudurru edge run` are milestone M4 deliverables;
until they are published use [../single](../single/). See
[docs/deploy/docker.md](../../../docs/deploy/docker.md).
