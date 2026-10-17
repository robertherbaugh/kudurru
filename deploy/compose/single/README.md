# Single instance with docker compose

```sh
mkdir -p secrets
openssl rand -base64 32 > secrets/encryption_key
openssl rand -base64 18 > secrets/admin_password
docker compose up -d
dig @127.0.0.1 example.com
curl -s localhost:8081/healthz      # inside the container network; or: docker compose exec kudurru kudurru health
```

Sign in at https://localhost:8080/ as `admin` with the password from
`secrets/admin_password`; the UI asks for a new one. The file is read only
while no user exists.

Zones, block lists, client groups and policies go into `objects.d/*.yaml`
(see `objects.d/README.md` and `deploy/kustomize/samples/`). The full guide is
[docs/deploy/docker.md](../../../docs/deploy/docker.md); dnsdist in front is
[../single-edge](../single-edge/).
