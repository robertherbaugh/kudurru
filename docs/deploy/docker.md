# kudurru in one container

The single-instance shape (dev-docs/design-record.md §4.4, §6): the sqlite store
in `/var/lib/kudurru/state.db`, configuration in `/etc/kudurru/kudurru.yaml`
plus `objects.d/`, the API and UI on :8080.

## docker run

```sh
mkdir -p kudurru/secrets && cd kudurru
openssl rand -base64 32 > secrets/encryption_key
openssl rand -base64 18 > secrets/admin_password
docker run -d --name kudurru --restart unless-stopped \
  -p 53:53/udp -p 53:53/tcp -p 8080:8080 \
  -v kudurru-data:/var/lib/kudurru \
  -v "$PWD/secrets:/run/secrets:ro" \
  -e KUDURRU_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key \
  -e KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  ghcr.io/robertherbaugh/kudurru:0.1.0
dig @127.0.0.1 example.com
```

kudurru runs without a license key. A key unlocks more than one node, more
than one policy and the features listed in [LICENSING.md](../../LICENSING.md);
add `-e KUDURRU_LICENSE_KEY_FILE=/run/secrets/license_key` (a file in the
secrets directory) or `-e KUDURRU_LICENSE_KEY=<key>`.

Without a mounted `kudurru.yaml` the built-in defaults apply: plain DNS on
:53 forwarding to 1.1.1.1 and 9.9.9.9, the starter blocklist, the query log.
The image runs as uid 65532 and binds :53 because Docker starts the
container's unprivileged port range at 0. With `--network host` the host's
`net.ipv4.ip_unprivileged_port_start` applies instead: run the
`-hostnetwork` image (`ghcr.io/robertherbaugh/kudurru:0.1.0-hostnetwork`,
the same binary with the file capability `cap_net_bind_service=+ep`), set the
sysctl to 53 on the host, or use the packages below. The `-hostnetwork`
image needs `NET_BIND_SERVICE` among the container's capabilities (Docker's
default set has it; with `--cap-drop ALL` add `--cap-add NET_BIND_SERVICE`),
otherwise it does not start ("exec /ko-app/kudurru: operation not
permitted").

## docker compose

`deploy/compose/single` is the reference: a read-only root filesystem, all
capabilities dropped, `no-new-privileges`, Docker secrets for the key and
the admin password, a named volume for `/var/lib/kudurru` and the health
check `kudurru health --ready`.

```sh
cp -r deploy/compose/single ~/kudurru && cd ~/kudurru
mkdir -p secrets
openssl rand -base64 32 > secrets/encryption_key
openssl rand -base64 18 > secrets/admin_password
docker compose up -d
docker compose logs -f kudurru
```

### First login

Open `https://<host>:8080/` and sign in as `admin` with the content of
`secrets/admin_password`. The UI asks for a new password. Without
`KUDURRU_ADMIN_PASSWORD_FILE` a password is generated and logged once at the
first start (`docker compose logs kudurru | grep -i password`). The file is
read only while no user exists; to recover a lost password use
`docker compose exec kudurru kudurru admin reset-password admin`.

Without a configured certificate the API and UI serve a self-signed one
(written to `keys/api.crt` in the data volume), so the browser warns on the
first visit; set `listeners.api.tls.certFile`/`keyFile` for a trusted one.

### Configuration

- `kudurru.yaml`: node keys (listeners, data directory, store, log) and the
  initial cluster settings (upstreams, cache, blocking, query log, rate
  limiting). `kudurru config schema` prints every key with its scope and
  flags; changes to keys flagged `R` need `docker compose restart kudurru`.
- `KUDURRU_SETTINGS_FILE` (`/var/lib/kudurru/settings.yaml` in the compose
  files): cluster settings changed in the UI, with `kudurru settings set` or
  `PATCH /api/v1/settings` are written here and layered over `kudurru.yaml`;
  the file is watched and hot keys apply without a restart. Without the
  variable such changes live in memory only (`persisted: false`).
- `objects.d/*.yaml`: zones, record sets, block lists, client groups and
  policies in the CRD format (`kubectl apply` YAML; examples in
  `deploy/kustomize/samples/`). They are file-managed (`managedBy: file:<path>`),
  re-applied on change, and read-only in the UI.
- Environment: `KUDURRU_<SECTION>__<KEY>` overrides scalar keys, for example
  `KUDURRU_LOG__LEVEL: debug`.

### Encrypted transports

Either let kudurru terminate them (add `listeners.dot`, `doh`, `doq`, `doh3`
with `tls.certFile`/`keyFile` to `kudurru.yaml`, mount the certificate and
publish 853/tcp, 853/udp, 443/tcp, 443/udp), or put dnsdist in front with
`deploy/compose/single-edge`: dnsdist owns :53/:853/:443 and the packet
cache, kudurru listens on `127.0.0.1:5353` in dnsdist's network namespace and
accepts PROXYv2 from loopback, so the query log keeps the real client
address. v0.1.0 includes the `kudurru-dnsdist` image; the supervisor needs
a license key with the edge entitlement (free for eligible personal use).

### Persistence and backups

Everything lives in the `kudurru-data` volume: `state.db` (objects, users,
tokens, audit), `querylog.db`, `local.db` (cache dump), `lists/` (blocklist
cache). Back up with the container stopped:

```sh
docker compose stop kudurru
docker run --rm -v kudurru_kudurru-data:/data -v "$PWD:/backup" busybox \
  tar -czf /backup/kudurru-data.tgz -C /data .
docker compose start kudurru
```

`kudurru backup --out objects.yaml` exports the objects (zones, record sets,
lists, client groups, policies) online and `kudurru restore -f objects.yaml`
re-applies them; it does not include users, tokens, the audit trail or the
query log, so keep the volume backup above for those.

### Metrics

Publish `127.0.0.1:9153:9153` (commented in the compose file) and scrape
`/metrics`. Import `deploy/grafana/kudurru-overview.json` into Grafana and
load the `spec.groups` of `deploy/prometheus/rules.yaml` as a rule file.

## Linux packages and systemd

The deb, rpm and apk packages install `/usr/bin/kudurru`, the unit
`/usr/lib/systemd/system/kudurru.service`, `/etc/kudurru/kudurru.yaml` and
`/etc/kudurru/kudurru.env` (both kept on upgrade) and create the `kudurru`
system user. The unit runs as that user with only `CAP_NET_BIND_SERVICE`,
`ProtectSystem=strict`, `StateDirectory=kudurru` (`/var/lib/kudurru`) and
`RuntimeDirectory=kudurru` (`/run/kudurru`). The package never starts the
service by itself:

```sh
sudo sh -c 'umask 027; openssl rand -base64 32 > /etc/kudurru/encryption_key'
sudo chgrp kudurru /etc/kudurru/encryption_key
sudo sed -i 's|^#KUDURRU_ENCRYPTION_KEY_FILE=|KUDURRU_ENCRYPTION_KEY_FILE=|' /etc/kudurru/kudurru.env
sudo systemctl enable --now kudurru
kudurru health --ready
```

If systemd-resolved holds :53 on the host, disable its stub listener
(`DNSStubListener=no` in `/etc/systemd/resolved.conf`) first.
