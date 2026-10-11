---
title: REST API reference
linkTitle: REST API
description: "The `/api/v1` contract: authentication, the response envelope, errors, paging and streaming. Every route is in the OpenAPI document."
weight: 40
hideChildren: true
---

The dashboard, the CLI and your automation use the same API. The OpenAPI
3.1 document is generated from the route registry (`make openapi`), and a
test fails when the committed copy drifts:

- [`openapi.json`](/reference/api/openapi.json) (OpenAPI 3.1, the document a
  server serves at `/api/v1/openapi.json`) and
  [`openapi-3.0.json`](https://github.com/robertherbaugh/kudurru/blob/main/api/openapi-3.0.json)
  for generators that need 3.0;
- the [endpoint index](endpoints.md): every route with the permission it checks;
- the [API explorer](explorer.md): request and response schemas, rendered by
  the same Scalar bundle a running server serves at `/api/v1/docs`.

## Authentication

| Credential | How to send it | Typical caller |
|---|---|---|
| API token `kdr_<id>_<secret>` | `Authorization: Bearer kdr_…` | scripts, CI, `kudurru login --token` |
| OIDC ID token from a configured provider | `Authorization: Bearer <jwt>` | workloads with an OIDC identity |
| Kubernetes ServiceAccount token | `Authorization: Bearer <jwt>`, authorized with a SubjectAccessReview | in-cluster automation |
| Session cookie `kudurru_session` | set by `POST /api/v1/auth/login` or the OIDC login; unsafe methods also send `X-Kudurru-CSRF` | the dashboard |

Tokens are created in the dashboard, with `POST /api/v1/auth/tokens`, or on
the host with `kudurru admin create-token`; the server stores only their
hash. Every route declares one permission (`resource.verb` evaluated at
cluster or zone scope), and a role binding grants roles that carry those
permissions: viewer, auditor, operator, zone-admin and admin. A 403 names the
missing permission in `error.missing`.

## Responses

- **Objects** (`/objects/{kind}/...`, `/apply`) are Kubernetes-shaped
  documents that re-apply verbatim; `kudurru apply -f` and `kubectl apply -f`
  take the same YAML. Writes carry the stored `resourceVersion` as an
  optimistic precondition, and `If-Match` turns a stale write into 412.
- **Lists** are `{items, continue, took_ms}`. Pass `limit`, and the opaque
  `continue` token to get the next page.
- **Errors** are `{error: {key, message, hint}, took_ms}`. The `key` is
  stable: `invalid`, `unauthenticated`, `forbidden`, `csrf`, `not_found`,
  `conflict`, `managed_by` (the hint names the owner, e.g.
  `file:objects.d/zones.yaml`), `precondition_failed`, `unprocessable`,
  `too_large`, `rate_limited`, `read_only`, `host_only`, `env_locked`,
  `not_implemented`, `unavailable`, `internal`.
- **Ownership.** Objects record who manages them (`api`, `file:<path>`,
  `integration:<name>`, Kubernetes). The API refuses to change an object
  another manager owns instead of fighting it.
- **Streams.** `GET /api/v1/queries/stream` is a Server-Sent Events feed of
  the query log; reconnect with `Last-Event-ID` to resume.
- **Zones** import and export as zone files (`text/dns`) on
  `/api/v1/zones/{zone}/import` and `/export`.

## Clients

- The CLI: `kudurru api GET /version` sends any request with the stored
  credentials of the current context ([CLI reference](/reference/cli/#kudurru-api)).
- Go: `internal/apiclient/openapi` is generated from `api/openapi-3.0.json`
  with oapi-codegen (`make client-go`).
- Anything else: generate a client from `openapi.json`.
