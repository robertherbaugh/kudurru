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
        A["license admin<br/>(also from a one-click mail link)"]
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
    F -->|"POST /api/v1/public/key-requests<br/>and /business-requests (CORS)"| API
    API --> PG[("Postgres<br/>users, sessions, customers,<br/>licenses, keys, requests,<br/>business requests, Order Forms,<br/>approval links, audit")]
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
    participant M as visitor's mailbox
    actor A as license admin
    V->>W: fill in the form, certify non-commercial use
    W->>P: POST /api/v1/public/key-requests (CORS)
    P->>P: limits per network and per address, the honeypot
    P-->>W: 202 {"status":"sent"}
    P->>M: confirmation link /verify?token=…
    V->>P: open the link, press Confirm (POST, so mail scanners cannot)
    P->>P: personal flow (below): open a request, sign nothing
    P-->>V: "Your request is waiting for approval"
    P->>A: approval mail: the details, the terms, /approve?token=… (one link per recipient)
    A->>P: open the link: the request and the terms (a POST preview, changes nothing)
    alt Approve and send the key
        A->>P: POST /api/v1/public/approvals/decide (the link is used up)
        P->>P: sign: renew the active personal key, or issue a new one
        P->>M: the key (license.key) and an invite link to the portal
    else Reject (with a note)
        A->>P: POST /api/v1/public/approvals/decide
        P->>M: the note
    end
```

The personal flow is shared with the portal's own "Request a free personal
key" and its renewal ("Request renewal", or "Renew" with
`PERSONAL_AUTO_APPROVE=true`). With `PERSONAL_AUTO_APPROVE=false`, the
default, it signs nothing: every new key and every renewal becomes a
request that a license admin approves.

```mermaid
flowchart TD
    R["request: name, email, use, attestation<br/>(the kudurru.io form after the confirmation,<br/>or the portal)"] --> B{"address blocked?"}
    B -->|yes| QB["open request: blocked"]
    B -->|no| W{"withdrawn license<br/>in the history?"}
    W -->|yes| QW["open request: withdrawn_history"]
    W -->|no| H{"holds an active<br/>personal key?"}
    H -->|"yes, not due yet"| RS["mail the current key again<br/>(nothing is signed)"]
    H -->|"yes, in its last 30 days,<br/>in grace or expired"| AA{"PERSONAL_AUTO_APPROVE?"}
    H -->|no| AA
    PR["the portal's renewal of a personal key<br/>(a fresh certification)"] -->|"in the renewal window"| AA
    AA -->|"false (the default)"| Q["open request: auto_approve_off<br/>(Approval required)"]
    AA -->|"true, a renewal"| RN["renew: +365 days, mail it"]
    AA -->|"true, a new key,<br/>under the daily cap"| IS["issue: personal, 3 nodes,<br/>every feature, 365 days; mail it"]
    AA -->|"true, a new key,<br/>daily cap reached"| QC["open request: daily_cap"]
    Q --> L["mail each admin: the details, a one-click<br/>approve link, the Requests page"]
    QC --> L
    QB --> N["mail each admin: the details and why,<br/>the Requests page only (no link)"]
    QW --> N
    L -->|"the link, or signed in"| D{"a license admin decides"}
    N -->|"signed in"| D
    D -->|approve| AP["renew the active personal key (+365 days)<br/>or issue a new one; mail the key to the requester"]
    D -->|reject| RJ["mail the note to the requester"]
```

A request while one is already open for the address is that request: no
second request and no second mail. The admins are the addresses in
`ADMIN_NOTIFY_EMAIL`, or, when it is unset, every enabled license admin and
superadmin with a verified email.

### The one-click link

```mermaid
sequenceDiagram
    autonumber
    participant P as portal
    participant DB as Postgres
    actor A as license admin
    actor R as requester
    P->>DB: per recipient: request_approval_links (SHA-256 of a random token, sent_to, expires in 7 days)
    P->>A: Approve a free personal key for (name)? with /approve?token=…
    A->>P: GET /approve?token=… (the UI, no session)
    A->>P: POST /api/v1/public/approvals/preview {token}
    P->>DB: the link by its hash, the request, the terms
    P-->>A: the request, the terms, the status (an expired or used link still shows them)
    A->>P: POST /api/v1/public/approvals/decide {token, decision}
    P->>DB: one transaction: the link (unused, unexpired) and the request (open) FOR UPDATE,<br/>the link used, then the same approve or reject as the Requests page, audited (via approval_link)
    P-->>A: approved or rejected (never the key)
    P->>R: the key, or the note, by mail
```

Opening the link never decides (mail scanners open links): the decision is
a POST from the page. The token is 32 random bytes, mailed once and stored
only as its SHA-256; it works for 7 days and once, and a request decided
any other way (on the Requests page, or through another recipient's link)
makes every link to it useless. A link also stops deciding when its
address no longer approves (it left `ADMIN_NOTIFY_EMAIL`, or its admin was
disabled or lost the role). The actor is the one enabled license admin or
superadmin whose verified email the link went to, and the approval counts
against their `ISSUE_MAX_PER_ACTOR_PER_DAY`; else it is the system actor
`approval link (<address>)`, which has no issuing quota. Both routes are
rate limited per client address. The preview answers for a used or expired
link too, until the cleanup deletes the link 30 days after its expiry.

## A business request from the kudurru.io form

```mermaid
sequenceDiagram
    autonumber
    actor V as visitor
    participant W as kudurru.io
    participant P as portal
    participant DB as Postgres
    actor A as license admin
    V->>W: Request a key: business (name, email, company, edition, nodes, details)
    W->>P: POST /api/v1/public/business-requests (CORS)
    P->>DB: limits per network and per email, then business_requests (open)
    P-->>W: 202 sent
    P->>V: mail: We have your kudurru business license request
    P->>A: mail per approver: Business license request from (company), link to Requests
    A->>V: reply by email (outside the portal)
    alt issue
        A->>P: Requests, Business: Issue license (the wizard filled in from the request)
        P->>DB: lock the open request, issue the license, request issued with the license
    else close
        A->>P: Close with a note
        P->>DB: request closed (nothing mailed)
    end
```

There is no confirmation link: nothing is signed until a license admin
issues a license, so the request is stored at once. When the portal cannot
be reached or cannot take it, the form opens an email to hello@kudurru.io
with the same fields instead.

## An Order Form

```mermaid
sequenceDiagram
    autonumber
    actor A as license admin
    participant P as portal
    participant DB as Postgres
    actor C as authorized contact
    A->>P: New Order Form, or Draft Order Form on a business request
    P->>DB: order_forms (draft, OF-year-number), fees from the price list
    A->>P: change the draft on its page, then Send
    P->>DB: status sent, SHA-256 of the document text, each contact a member
    P->>C: mail: the form's link (and an invite link without access)
    C->>P: sign in, read the form
    alt accept
        C->>P: POST /api/v1/me/order-forms/:id/accept (name, title, the SHA-256 read)
        P->>DB: status accepted, the acceptance with time, address and user agent
        P->>A: mail per approver: countersign it
        A->>P: Countersign (name, title, the same SHA-256)
        P->>DB: status executed (final)
        P->>C: mail: the signed copy (OF-year-number.md)
        A->>P: Issue license (the wizard filled in from the form)
        P->>DB: the license (within the form's customer, plan, nodes, features and term), the form's license_id, its business request issued
    else decline (with a note)
        C->>P: POST /api/v1/me/order-forms/:id/decline
        P->>A: mail per approver: the note
        A->>P: Revise (draft again, the next version) or Void
    end
```

```mermaid
stateDiagram-v2
    [*] --> draft: create
    draft --> sent: send (text frozen and hashed)
    sent --> accepted: an authorized contact accepts that hash
    sent --> declined: an authorized contact declines
    sent --> draft: revise (next version)
    declined --> draft: revise (next version)
    accepted --> executed: countersign
    draft --> void: void
    sent --> void: void
    accepted --> void: void
    declined --> void: void
    executed --> [*]: one license may be issued from it
```

The text a party accepts is `orderFormDocument` without signatures
(`shared/orderForms.ts`), stored when the form is sent; every kept copy
is that stored text with the acceptances appended. Both acceptances record
its SHA-256, and the
accept and countersign bodies name the hash the person saw, so an answer
to a revised form's earlier text is refused (409). Only a signed-in member
of the customer whose membership address is one of the form's authorized
contacts answers it; other members see it. Customers never see drafts,
void reasons or who did what in the back office.

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
    license_requests ||--o{ request_approval_links : "one-click links (one per recipient)"
    public_key_requests }o--o| license_requests : "may become (when it waits for approval)"
    business_requests }o--o| licenses : "issued as"
    customers ||--o{ order_forms : "agrees"
    business_requests |o--o{ order_forms : "drafted from"
    order_forms |o--o| licenses : "issued as"
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
    license_requests {
        text id
        text source "web or portal"
        text requester_email
        jsonb attestation
        text review_reason "auto_approve_off daily_cap blocked withdrawn_history"
        text status "open approved rejected canceled"
    }
    business_requests {
        text id
        text email
        text company
        text edition "core or enterprise"
        int nodes
        text status "open issued closed"
    }
    order_forms {
        text id
        text number "OF-year-number"
        text status "draft sent accepted executed declined void"
        int version
        jsonb terms "Schedule A"
        text document_sha256 "of the text sent"
        text document_text "the text sent"
        jsonb customer_acceptance
        jsonb licensor_acceptance
        text license_id "one per form"
    }
    request_approval_links {
        bytea token_hash "SHA-256 of the token"
        text request_id
        text sent_to
        timestamptz expires_at "7 days"
        timestamptz used_at "single use"
    }
    audit_events {
        bigint id
        text action
        text target_id
        jsonb details "never a token"
    }
```

`audit_events` refuses UPDATE, DELETE and TRUNCATE by trigger, and the
runtime role may only read and append it (migration 0002). The cleanup
job deletes approval links 30 days after they expire (migration 0003 adds
the table and its grant). Deleting a user (migration 0004 grants it)
deletes their sessions and memberships; every other reference keeps its
label and loses the link. Migration 0005 adds `business_requests`, 0006
`order_forms` and its number sequence (forms are never deleted).

## Roles

| Permission | customer | license admin | superadmin |
|---|---|---|---|
| Own licenses and keys (members of a customer), personal key requests, asking for the renewal of a personal key, accepting invites, the Order Forms of member customers (accepting and declining them as an authorized contact) | yes | yes | yes |
| Customers, members, licenses (issue, import, renew, change, withdraw, email), Order Forms (draft, change, send, revise, countersign, void), requests (approve, reject; business requests: issue, close), the block list, users (read), audit, system | | yes | yes |
| Granting roles, disabling and deleting users, ending sessions, removing block list entries | | | yes |

`shared/permissions.ts` is the source; every route in `shared/routes.ts`
names the one permission it needs, and `server/test/rbac.test.ts` checks
each route against each role. The one-click approval routes are anonymous:
the token in the link is the credential, not a role.
