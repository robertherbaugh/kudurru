---
title: Automate with the API and the CLI
linkTitle: API and CLI
audience: API and CLI user
description: One REST API behind the dashboard, the CLI and your scripts, with scoped tokens, Kubernetes-shaped objects and a generated OpenAPI document.
weight: 40
---

Everything the dashboard does goes through `/api/v1`. The `kudurru` CLI uses
that API by default, with an explicit direct Kubernetes mode for generic CRD
commands. Objects travel as the same Kubernetes-shaped YAML everywhere,
so a file that `kubectl apply` accepts in a cluster is what
`kudurru apply -f` sends to a single container.

## Log in once

```sh
kudurru login --server https://dns.example:8080 --username admin   # mints a token for the CLI
kudurru login --server https://dns.example:8080 --token-stdin < token.txt
kudurru config get-contexts
```

`login` saves a named context in `~/.config/kudurru/config.yaml` and keeps
the token in the OS keyring (or the 0600 config file where there is none).
Flags (`--server`, `--token`, `--context`) and the environment
(`KUDURRU_SERVER`, `KUDURRU_TOKEN`, `KUDURRU_CONTEXT`) override the context,
which suits CI. A server with the default self-signed certificate needs
`--ca-file`, or `--insecure-skip-tls-verify` in a lab.

## Everyday commands

```sh
kudurru get zones                          # also: rrset, blocklist, cg, pol; -o yaml|json|wide
kudurru apply -f objects/ -R               # create or update, kubectl-style
kudurru records add home-example-com nas.home.example.com A 192.168.1.10
kudurru query ads.example.net --client 192.168.1.23   # the verdict for that client
kudurru logs --follow                      # the query log as it happens
kudurru settings set cache.maxEntries=200000
kudurru backup --out backup.yaml           # every object, re-appliable with restore
kudurru api GET /stats/summary             # any route, with the context's credentials
```

The [CLI reference](/reference/cli/) lists every command and flag;
`kudurru <command> --help` prints the same text.

## Direct Kubernetes CRD commands

Choose this transport explicitly; existing HTTP contexts keep their meaning:

```sh
kudurru --transport kubernetes --kubeconfig /path/to/config --context lab get zones
kudurru --transport kubernetes --context lab -n dns apply -f objects.yaml
kudurru --transport kubernetes --context lab get cg -A -l 'team in (home,lab)'
kudurru --transport kubernetes --context lab describe zone home-example-com
```

Without `--kubeconfig`, standard `KUBECONFIG` paths or `~/.kube/config` are used.
The selected kube context supplies credentials, TLS trust, optional impersonation
and the default namespace; `-n` overrides that namespace. Only generic `get`,
`apply`, `delete` and `describe` are supported for the five existing public kinds
(zones, record sets, block lists, client groups and policies). Other command
families require the HTTP API. Direct `describe` shows CRD spec/status, without
server diagnosis results.

API `--server`/`--token`/CA/insecure overrides, `KUDURRU_SERVER`/`KUDURRU_TOKEN`
and explicit `--cli-config` cannot be combined with Kubernetes transport.
No HTTP token is forwarded or context silently reinterpreted. Treat kubeconfig,
including its executable credential plugins, as trusted host configuration.
Kubernetes RBAC and admission apply; no Kudurru role binding grants Kubernetes
permissions. Direct writes retain manager `api` ownership and original absence,
resource-version or UID preconditions. There is no force/adoption option or
retry against a replacement object. Apply documents have independent outcomes,
not a batch transaction.

`--request-timeout` bounds the entire direct command (default 30s, maximum 5m).
Responses are capped at 16 MiB; listings at 10,000 objects, 64 MiB and 200 pages.
Narrow the namespace or selector if a bound is reached. An incomplete listing is
an error. No informer, election, private Secret or background server is started.

## Tokens for automation

Create a token for each job, with an expiry, in the dashboard or with
`POST /api/v1/auth/tokens`. A token acts with its owner's role bindings,
which can be scoped to a single zone (a `zone-admin` of `home-example-com`
manages that zone's records and nothing else), and optional `scopes` narrow
it further. For
the first automation of a fresh install, `kudurru token generate` makes a
token offline that the server accepts at its first start through
`KUDURRU_BOOTSTRAP_TOKEN_FILE`. On the host, `kudurru admin create-token`
mints a short-lived break-glass token over the local admin socket without
any credentials.

## The API itself

- [REST API reference](/reference/api/): authentication, the envelope,
  error keys, paging, ownership, streams.
- [Endpoint index](/reference/api/endpoints/): every route and the
  permission it checks.
- [API explorer](/reference/api/explorer/): schemas of every request and
  response.
- [CRD reference](/reference/crds/): the objects you send to `/apply` and
  `/objects/{kind}`.
