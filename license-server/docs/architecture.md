# How the license portal works

The license portal issues kudurru license keys and hands them to the people
they belong to. It never talks to a kudurru node, and no node ever talks to
it: a node checks a key's signature offline against the public keys built
into its binary (`docs/architecture/licensing.md`, D163). Operating detail
is in [../README.md](../README.md); this page shows the flows.

Code: `server/` (Bun), `ui/` (React, Astryx), `shared/` (what both use),
`cloudflare/` (the Worker in front of the container on Cloudflare).

## Components

```mermaid
flowchart LR
    subgraph people["people"]
        direction TB
        C["customer"]
        A["license admin"]
        S["superadmin"]
        F["anyone on kudurru.io"]
    end
    subgraph portal["license portal (one container)"]
        direction TB
        UI["UI<br/>React + Astryx"]
        API["/api/v1<br/>router from shared/routes.ts"]
        SIGN["signer<br/>Ed25519, in memory"]
        UI --> API
        API --> SIGN
    end
    C --> UI
    A --> UI
    S --> UI
    F -->|"POST /api/v1/public/key-requests<br/>(CORS)"| API
    API --> PG[("Postgres<br/>users, sessions, customers,<br/>licenses, keys, requests, audit")]
    API -->|"SMTP"| MAIL["mail server"]
    API -->|"OIDC (code + PKCE)"| IDP["identity provider"]
    SIGN -. "keys by mail and download" .-> N["kudurru nodes<br/>(verify offline)"]
```

The signing key comes from a secret at run time (a file, or an environment
variable on Cloudflare) and stays in memory. Postgres holds the record:
who holds which license, every key signed, the attestations and the
append-only audit trail.

## Sign-in

```mermaid
sequenceDiagram
    autonumber
    actor B as browser
    participant P as portal
    participant DB as Postgres
    participant I as identity provider
    B->>P: GET /api/v1/auth/login?next=/licenses
    P->>DB: oidc_logins (state, nonce, PKCE verifier, hash of the binding cookie)
    P-->>B: 302 to the IdP, Set-Cookie __Host-kls_login
    B->>I: sign in
    I-->>B: 302 /api/v1/auth/callback?code&state
    B->>P: callback (with the binding cookie)
    P->>DB: delete the oidc_logins row by state (single use), check the binding
    P->>I: code + verifier for tokens (client secret)
    P->>P: verify the ID token (signature, iss, aud, exp, nonce, azp)
    P->>DB: upsert the user by (iss, sub), roles from groups and grants
    P->>DB: new session (SHA-256 of the cookie secret, CSRF token)
    P-->>B: 302 next, Set-Cookie __Host-kls_session
    B->>P: GET /api/v1/auth/me (the UI keeps the CSRF token from it)
```

A failure ends at `/login?error=<key>` with a fixed sentence; the cause is
only logged.

## A personal key from the kudurru.io form

```mermaid
sequenceDiagram
    autonumber
    actor V as visitor
    participant W as kudurru.io
    participant P as portal
    participant M as mailbox
    V->>W: fill in the form, certify non-commercial use
    W->>P: POST /api/v1/public/key-requests (CORS)
    P->>P: limits per network and per address, the honeypot
    P-->>W: 202 {"status":"sent"}
    P->>M: confirmation link /verify?token=…
    V->>P: open the link, press Confirm (POST, so mail scanners cannot)
    P->>P: personal flow: review? renew? resend? issue
    P->>M: the key (license.key) and an invite link to the portal
```

The personal flow is shared with the portal's own "Request a free personal
key":

```mermaid
flowchart TD
    R["request: name, email, use, attestation"] --> B{"address blocked?"}
    B -->|yes| Q["wait for review"]
    B -->|no| W{"withdrawn license<br/>in the history?"}
    W -->|yes| Q
    W -->|no| H{"holds an active<br/>personal key?"}
    H -->|"yes, in its last 30 days<br/>or expired"| RN["renew: +365 days, mail it"]
    H -->|"yes, otherwise"| RS["mail the current key again"]
    H -->|no| AA{"automatic approval on<br/>and under the daily cap?"}
    AA -->|no| Q
    AA -->|yes| IS["issue: personal, 3 nodes,<br/>every feature, 365 days; mail it"]
    Q --> AD["an admin approves (issue) or rejects (mail the note)"]
```

## A license's keys

```mermaid
stateDiagram-v2
    [*] --> active: issue (or import a hand-signed key)
    active --> active: renew (later expiry) / change (terms, customer)
    active --> withdrawn: withdraw
    withdrawn --> [*]
    note right of active
        Every renew or change signs a new key with the same
        License ID (jti) and supersedes the old one; exactly one
        key is current. Old keys keep working on nodes until
        their own expiry plus 30 days.
    end note
    note right of withdrawn
        The portal stops handing the key out and renewing it.
        Installed keys keep working until the latest expiry
        plus 30 days: only rotating the signing key stops them.
    end note
```

What the server state shows follows the node: active until `exp`,
"expires soon" in the last 30 days, "in grace" for 30 days after `exp`,
then expired. `exp` is 23:59:59 UTC of the chosen day, so the portal and
`kudurru license status` print the same date.

## Data model

```mermaid
erDiagram
    users ||--o{ sessions : has
    users |o--o{ customer_members : "linked as"
    customers ||--o{ customer_members : "people with access"
    customers ||--o{ licenses : holds
    licenses ||--|{ license_keys : "signed keys (one current)"
    license_requests }o--o| licenses : "results in"
    public_key_requests }o--o| license_requests : "may wait as"
    users {
        text id
        text issuer
        text subject
        text email
        text_array idp_roles
        text_array granted_roles
    }
    licenses {
        text id "the jti"
        text licensee
        text plan
        int nodes
        text_array features "null: every feature"
        timestamptz expires_at
        text status "active or withdrawn"
        jsonb attestation
    }
    license_keys {
        text id
        text kid
        text token
        text reason "issue renew change import"
        timestamptz superseded_at
    }
    audit_events {
        bigint id
        text action
        text target_id
        jsonb details "never a token"
    }
```

`audit_events` refuses UPDATE, DELETE and TRUNCATE by trigger, and the
runtime role may only read and append it (migration 0002).

## Roles

| Permission | customer | license admin | superadmin |
|---|---|---|---|
| Own licenses and keys (members of a customer), personal key requests, renewing a personal key, accepting invites | yes | yes | yes |
| Customers, members, licenses (issue, import, renew, change, withdraw, email), requests, the block list, users (read), audit, system | | yes | yes |
| Granting roles, disabling users, ending sessions, removing block list entries | | | yes |

`shared/permissions.ts` is the source; every route in `shared/routes.ts`
names the one permission it needs, and `server/test/rbac.test.ts` checks
each route against each role.
