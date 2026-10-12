# Clients, policies and blocking

Blocking in kudurru is per client. Two stages decide it: `identify` works out
who is asking and which policy applies, and `blocking` judges the name
against that policy's lists. Between them sit four kinds of object:

```mermaid
erDiagram
    ClientGroup ||--o{ ClientMatcher : "spec.clients[]"
    Policy }o--o{ ClientGroup : "clientGroupRefs"
    Policy }o--o{ BlockList : "blockListRefs (mode block)"
    Policy }o--o{ BlockList : "allowListRefs (mode allow)"
    Policy }o--o{ CatalogueService : "blockedServices / blockedCategories"
    BlockList ||--|{ Source : "sources[]: url | inline | configMapRef | entries"
    ClientMatcher {
        string ip
        string cidr
        string mac "not filled in yet"
        string hostname "not filled in yet"
        string clientID "not filled in yet"
    }
    Policy {
        int priority
        object schedule
        bool allowOnly
        string blockMode
        duration blockTTL
        string upstreamGroup
        bool safeSearch
    }
```

Code: `internal/policy` (the index, matching, merging, schedules),
`internal/stages/identify`, `internal/stages/blocking`, `internal/blocking`
(the compiled lists and the matchers), `internal/domainlist`,
`internal/unblock`, `internal/blockpage`, `internal/listdata/catalogue`.

![From a client address to a verdict: identify by tiers, collect the group's policies, merge them, then evaluate the name](diagrams/policy-decision.excalidraw.svg)

## 1. Who is asking

The policy index is rebuilt (as part of the zonecache snapshot) whenever a
ClientGroup, Policy or BlockList changes. At query time `Identify` walks
**tiers** and stops at the first tier with a hit; only that tier's groups
are used. A client in an IP group and a CIDR group belongs to the IP group
only. Two groups that claim the same address in the same tier both apply
(and the index reports a `Tie`).

```mermaid
flowchart TD
    C(["client address<br/>(after PROXYv2)"]) --> IP{"exact IP<br/>in a group?"}
    IP -- "yes" --> G["those groups"]
    IP -- "no" --> MAC{"MAC?"}
    MAC -- "yes" --> G
    MAC -- "no" --> CIDR{"longest matching<br/>CIDR prefix?"}
    CIDR -- "yes" --> G
    CIDR -- "no" --> HN{"hostname /<br/>name glob?"}
    HN -- "yes" --> G
    HN -- "no" --> CID{"client ID?"}
    CID -- "yes" --> G
    CID -- "no" --> CATCH{"a group with<br/>0.0.0.0/0 or ::/0?"}
    CATCH -- "yes" --> G
    CATCH -- "no" --> DEF["group default<br/>(if it exists)"]
    classDef dead fill:#e6e9e8,stroke:#5b6664,color:#5b6664
    class MAC,HN,CID dead
```

The grey tiers exist in the index, but nothing feeds them yet: no transport
sets a MAC or a DoT/DoH client ID, and no hostname resolver is wired. Today
clients are matched by exact IP, by CIDR, or fall through to the catch-all.
The bootstrap `ClientGroup default` lists `0.0.0.0/0` and `::/0`, which is
how "default matches everybody" works.

## 2. Which policy applies

Every Policy attached to any of the client's groups is a candidate. The
index precomputes the merged result for each combination of scheduled
policies being on or off, so the query path only picks the right one.

```mermaid
flowchart TD
    G(["client's groups"]) --> CAND["candidates: policies attached to those groups<br/>sorted by priority (high first), then namespace/name"]
    CAND --> SCHED["drop policies whose schedule<br/>is not active at the query time"]
    SCHED --> ANY{"any left?"}
    ANY -- "no" --> DEFP{"Policy default<br/>exists and active?"}
    DEFP -- "yes" --> MERGE
    DEFP -- "no" --> NOF["no filtering at all"]
    ANY -- "yes" --> MERGE["merge in order"]
    MERGE --> R1["allowOnly: from the top policy only"]
    MERGE --> R2["blockMode, blockTTL, upstreamGroup,<br/>safeSearch, safeSearchMode:<br/>first policy that sets a value wins"]
    MERGE --> R3["lists: allow lists of every active policy,<br/>then block lists and service lists of every<br/>active policy, de-duplicated; each list goes<br/>to the allow or block side by its own mode"]
```

A schedule is `{weekdays, start, end, tz}`. An `end` earlier than `start`
crosses midnight and belongs to the start day; `start == end` means 24 hours;
no weekdays means every day. A schedule that does not parse makes the policy
**always active** (it fails closed) and raises a diagnosis.

`queryLog` is merged into the decision but nothing reads it yet, and
`rewrites` is ignored (planned for v0.2).

## 3. Is this name blocked?

```mermaid
flowchart TD
    D(["policy decision on the request"]) --> AP{"a policy applies?"}
    AP -- "no" --> PASS["next stage, unfiltered"]
    AP -- "yes" --> UG["apply the policy's upstreamGroup<br/>(only over an empty or default hint)"]
    UG --> EN{"filters, and blocking<br/>enabled on this node?"}
    EN -- "no" --> PASS
    EN -- "yes" --> AL{"any allow list<br/>matches the name?"}
    AL -- "yes" --> UNT["next stage, untouched<br/>(no safe search, no answer check)"]
    AL -- "no" --> AO{"allowOnly?"}
    AO -- "yes" --> BLK17["block, EDE 17 Filtered"]
    AO -- "no" --> BL{"for each block list in order:<br/>a deny rule matches and that same<br/>list has no @@ exception?"}
    BL -- "yes" --> BLK15["block, EDE 15 Blocked"]
    BL -- "no" --> SS{"safe search on and the<br/>name is a search engine?"}
    SS -- "yes" --> SSA["CNAME to the safe host,<br/>resolved through the cache<br/>blocked_safesearch, EDE 4"]
    SS -- "no" --> NEXT["next stage (cache → upstream)"]
    NEXT --> RESP{"NOERROR answer: a CNAME or<br/>SVCB/HTTPS target, or an A/AAAA<br/>address, on a block list?"}
    RESP -- "yes" --> BLKC["block, blocked_cname"]
    RESP -- "no" --> OUT["the answer"]
```

Rules worth remembering:

- **Allow wins across policies.** An allow list of any active policy beats
  every block list of every active policy.
- **`@@` exceptions are per list.** An exception in list A does not cancel a
  match in list B.
- **The answer is checked too.** Up to 64 names and addresses from the
  answer are checked against the same lists (an allow match skips that
  item), so a CNAME into a tracker domain is blocked even when the asked
  name is clean. This also applies to cached answers.

### What a block looks like

The mode comes from the BlockList that matched (`spec.response`), else the
policy's `blockMode`, else `blocking.defaultResponse`:

| Mode | A / AAAA | Other types |
|---|---|---|
| `zeroIP` (default) | `0.0.0.0` / `::` | NODATA |
| `nxdomain` | NXDOMAIN (no SOA) | NXDOMAIN |
| `customIP` | the list's or the configured address | NODATA |
| `blockPage` | the block page addresses (zeroIP when none is set) | NODATA |
| `refused` | REFUSED | REFUSED |

The TTL is the policy's `blockTTL`, else `blocking.blockTTL`, else 10
minutes. When the client sent EDNS, the answer carries EDE 15 (or 17 for
allow-only) with the list name as extra text. The query log records the
list (`namespace/name` or `service:<id>`) and the rule, which is how the
dashboard explains every block. The status is `blocked_regex` for a regex
rule, `blocked_exact` for an exact or suffix rule from a list made only of
inline entries, and `blocked_list` otherwise.

### Turning blocking off for a while

`POST /api/v1/dns/blocking {blocking: false, timer: 300}` disables blocking
**on the node that receives the request**, in memory, until the timer ends
or the process restarts. The API reports `scope: "node"`. The cluster-wide
toggle that the design record describes is not built: the replication hook
exists, but nothing implements it.

## Blocked services and safe search

The blocked-services catalogue is a kudurru-authored JSON document embedded
in the binary (and optionally replaced from `blocking.catalogue.url`, which
the leader downloads and the other nodes pull over `:8082`, like a list).

- Each **service** compiles to a block list named `service:<id>`, in the
  same compiled snapshot as the BlockLists. A policy's `blockedServices`
  plus the services of its `blockedCategories`, minus `allowedServices`,
  are appended after its block lists.
- Each **search engine** has a safe host. With `safeSearch` on, a query for
  the engine's name is answered with a CNAME to the safe host (`strict` or,
  where the engine has one, `moderate`), and kudurru resolves the target
  itself so the client gets addresses in one answer.

## Allow and deny from the dashboard

The **Allowlist** and **Denylist** pages do not edit policies directly. They
write structured entries into four managed BlockLists, and a leader loop
keeps those lists attached to the right policies:

| List | Holds | Attached to |
|---|---|---|
| `ui-allow` / `ui-deny` | entries for everyone | every writable policy in the namespace |
| `ui-allow-<group>` / `ui-deny-<group>` | entries for one group | the policies that name that group |

Policies owned by files or integrations are never edited; they are reported
as skipped.

## The block page and unblock requests

With `blockMode: blockPage`, a blocked web site resolves to the block page
listener. The page explains the block and lets the user ask for an
exception; an administrator approves it in the dashboard.

```mermaid
sequenceDiagram
    autonumber
    actor U as user (10.0.20.15)
    participant D as kudurru DNS
    participant BP as block page :80
    participant Q as query log / policy index
    participant S as store
    actor A as administrator
    U->>D: A ads.example
    D-->>U: A = blockPage.ipv4
    U->>BP: GET / (Host: ads.example)
    BP->>Q: why? query log tail (10 min), else re-evaluate the policy
    BP-->>U: "blocked by dns/ads: ||ads.example^" + request form
    U->>BP: POST /.kudurru/request (same origin, rate limited)
    BP->>S: Apply UnblockRequest ur-<HMAC(client, domain)> (Pending)
    A->>S: approve (scope everyone or group, exact or subdomains, optional duration)
    S->>S: add the entry to ui-allow[-group], attach it to the policies
    Note over S,D: every node: list recompiles → new snapshot → allow list wins
    A->>D: the API flushes the name from this node's cache
    U->>D: A ads.example (after the old block TTL)
    D-->>U: the real address
```

- One request object exists per (client, domain): the name is an HMAC of
  both, so repeated submissions update one object. The client address is
  stored according to `querylog.privacy`.
- A leader janitor runs every minute: time-limited approvals expire (and
  their allow entry is removed), pending requests expire after 14 days, and
  decided requests are deleted after 30 days.
- The block page finds the reason in the query log first. Under the
  stricter privacy modes it cannot, and falls back to evaluating the policy
  for the client's address (which cannot see CNAME blocks).

Next: [Block lists](block-lists.md) shows how the lists behind all of this
are fetched and compiled.
