# license-server/dev

Configuration and local state for the development stack in
[../compose.yaml](../compose.yaml) (Postgres, Dex, Mailpit and the server's
image) and for a server run on the host with `bun run dev`.

| Path | What it is |
|---|---|
| `dex.yaml` | Dex configuration: the issuer `http://localhost:5556/dex`, the client `license-portal` and four static users |
| `server.env` | settings of a server run on the host (`make license-server-dev`) |
| `secrets/` | gitignored; written by `bun scripts/dev-keygen.ts` |
| `secrets/signing.key` | a development signing key (licensegen format: standard base64 of the 32-byte Ed25519 seed), mode 0600 |
| `secrets/oidc_client_secret` | the client secret `dex.yaml` registers (a fixed throwaway value) |
| `trusted-keys/` | gitignored; `<kid>.pub` of the development key, the only key a dev server trusts |

## Before the first start

```sh
cd license-server
bun scripts/dev-keygen.ts      # kid dev-1; --kid, --out and --force change that
docker compose --profile server up -d --build --wait
```

`make license-server-up` (from the repository root) does the same, but
builds the image with `docker build` first: compose's own build needs
buildx 0.17 or later.

`dev-keygen` keeps existing key files: replacing the key makes every key
issued with the old one fail to verify. Pass `--force` to replace it anyway.

No kudurru release trusts the development key. The compose server runs in
dev mode (`LICENSE_SERVER_DEV=true`) with `LICENSE_TRUSTED_KEYS_DIR`
pointing at `trusted-keys/` instead of the image's keyring, so nothing it
signs can pass for a real key, and the UI shows a development banner.

## Users

Sign in at http://localhost:8791 (compose) or http://localhost:5174 (Vite on
the host) with an email address below and the password
`correct horse battery staple`.

| User | Dex group | Role |
|---|---|---|
| superadmin@example.org | license-superadmins | superadmin |
| admin@example.org | license-admins | license-admin |
| customer@example.org | none | customer (the default role) |
| other@example.org | none | customer (the default role) |

Mail the server sends lands in Mailpit: http://localhost:8025.

## A server on the host

`make license-server-dev` (from the repository root) starts Postgres, Dex
and Mailpit in compose and runs the Bun server on 8790 and Vite on
http://localhost:5174 with the settings in `server.env`, the compose
service's settings adjusted for the host. By hand, from license-server/:

```sh
docker compose up -d --wait postgres dex mailpit
bun --env-file=dev/server.env run dev
```
