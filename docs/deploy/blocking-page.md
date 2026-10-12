# Block page

With the `blockPage` block response, a blocked name does not resolve to
`0.0.0.0` but to the address of a small web server on the kudurru node.
A browser that opens the name lands on a kudurru page that says the name is
blocked, why, and whom to ask, instead of a connection error.

```
browser ── A www.ads.example? ──▶ kudurru ── A 192.168.1.10 (EDE 15 "default/ads")
browser ── GET / Host: www.ads.example ──▶ 192.168.1.10:80 (block page) ── 200 "This site is blocked"
```

## What the response answers

`blockPage` is accepted everywhere a block response is: `blocking.defaultResponse`,
`Policy.spec.blockMode` and `BlockList.spec.response` (the BlockList's own
response wins, then the Policy's, then the default).

| Query type | Answer |
|---|---|
| A | `blockPage.ipv4` (NODATA when there is none) |
| AAAA | `blockPage.ipv6` (NODATA when there is none) |
| any other (HTTPS, SVCB, MX, ...) | NODATA |

The answer carries EDE 15 (Blocked) with the list as extra text, or EDE 17
(Filtered) for allow-only policies, and the block TTL (`Policy.spec.blockTTL`,
else `blocking.blockTTL`, 10 m). With the block page off (`blockPage.enabled:
false`) or without any address, `blockPage` answers like `zeroIP`.

## Setup

1. Pick the address clients are sent to. On a single host that is the host's
   LAN address; in Docker the **host's** address (the container address is
   not reachable by clients); in Kubernetes the address of the block page
   Service (a LoadBalancer IP).
2. Enable the page and choose the response (cluster settings: the settings
   file, the Settings page or `kudurru settings set`):

   ```yaml
   blockPage:
     enabled: true          # restart required: it starts the listener
     ipv4: 192.168.1.10
     # ipv6: 2001:db8::10
     title: This site is blocked
     message: The administrator of this network has blocked access to this domain.
     contact: it@example.net        # or https://intranet.example/helpdesk
     showDetails: true              # list, rule, service and policy on the page
   blocking:
     defaultResponse: blockPage     # or per Policy / BlockList
   ```

3. The listener is a node key in kudurru.yaml (host only):

   ```yaml
   listeners:
     blockPage:
       http: ":80"          # default
       https: ""            # optional, e.g. ":443"; read "HTTPS" below
       # tls: {certFile: ..., keyFile: ...}   # else a self-signed pair in <dataDir>/keys/blockpage.crt
   ```

4. Check it:

   ```sh
   dig @192.168.1.10 www.ads.example A +short     # 192.168.1.10
   curl -sI -H 'Host: www.ads.example' http://192.168.1.10/ | grep -i x-kudurru-block
   ```

   Every response of the block page carries `X-Kudurru-Block: 1` for scripted
   checks. The dashboard's Settings page shows the page with the current
   settings (`GET /api/v1/blockpage/preview?domain=...`, permission
   `settings.read`; the preview uses an example reason and reads neither the
   query log nor the policies).

When `blockPage.ipv4` and `blockPage.ipv6` are empty, kudurru uses the first
specific (not `0.0.0.0` / `::`) address of `listeners.blockPage`, else of
`listeners.dns`, skipping loopback when a PROXYv2 edge sits in front. When
there is none, `blockPage.enabled` (and `blocking.defaultResponse:
blockPage` without `blockPage.enabled`) is refused at start and by
`PATCH /api/v1/settings`, with a message naming the keys to set.

### Docker

`deploy/compose/single` has the lines commented out: publish the block page
port (`"80:8090"`, the container listens on `:8090` so it runs unprivileged),
set `listeners.blockPage.http: "0.0.0.0:8090"`, and set `blockPage.ipv4` to
the host's LAN address. Host port 80 must be free.

### Kubernetes (Helm)

```yaml
blockPage:
  enabled: true
  ipv4: 192.0.2.80            # the Service's load-balancer address
  https: {enabled: false}
  service:
    type: LoadBalancer
    annotations:
      metallb.io/loadBalancerIPs: 192.0.2.80   # or kube-vip.io/loadbalancerIPs
config:
  settings:
    blocking: {defaultResponse: blockPage}
```

The chart renders `listeners.blockPage` (container ports 8090 and 8493),
`blockPage.enabled/ipv4/ipv6` in the settings ConfigMap (`config.settings.blockPage`
still wins), a `<release>-blockpage` Service on ports 80/443 that selects the
kudurru pods, and, with `networkPolicy.enabled`, a rule admitting every client
to the block page ports. `blockPage.enabled` without `ipv4` or `ipv6` fails
the render.

## The page

The page answers every path of every Host name: the blocked name is the Host
header (or the TLS server name), lower-cased and checked to be a DNS name;
anything else (an IP literal, markup) is shown as "no domain". It shows:

- the blocked name, the title and message, the client address, the node name
  and the time;
- with `showDetails: true`, why: the list (`default/ads`), the rule
  (`||ads.example^`), the blocked service (`service:tiktok`), safe search,
  an allow-only policy, or a CNAME/answer block, and the client's policy;
  plus a link for administrators to `<dashboard>/queries?domain=<name>`
  (`blockPage.dashboardURL`, else the API listener at the block page address);
- "Ask your administrator", with `blockPage.contact` as a `mailto:` or http(s)
  link;
- a Request access form (`unblockRequests.enabled`, see "Unblock requests"
  below).

The reason is the most recent block of that name for that client within ten
minutes: first the query log's in-memory tail (which has the query even before
the database flush), then `querylog.db`. The query-log privacy mode limits the
lookup: `full` matches client and name; `anonymize_client` matches the client's
/24 or /48 (prefix) or its daily pseudonym; `hide_domains` and stricter
modes, and a disabled query log, keep no name to match. Under `prefix`, a
device can therefore see the reason of a block another device of its network
got for the same name in those ten minutes. Without a query-log
row, the name is evaluated against the policy that applies to the client
address now (what the blocking stage does for the question; response-side
blocks are not visible that way). Without either, the page says why no reason
is shown.

`/favicon.ico`, `/robots.txt` (`Disallow: /`) and `/health` (`ok`) are served
too. GET and HEAD answer 200; POST is accepted only on `/.kudurru/request` (the
request form), every other method and path answers 405. Each client address
(IPv6 per /64) gets `blockPage.rateLimit` requests per minute (default 120,
0 disables it); beyond it the page answers 429 with `Retry-After`. `/health`
is never limited. The listener never proxies or fetches anything.

The page is self-contained: the logo is inlined, the style sheet is allowed by
its hash, nothing loads from elsewhere and nothing runs:
`Content-Security-Policy: default-src 'none'; style-src 'sha256-...';
img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors
'none'`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy:
same-origin` (no referrer leaves for other sites; the form's POST carries its
`Origin`). Every value (Host, settings, list names, rules, request text and
notes) is HTML-escaped; the page is written by a small hand-written renderer,
not `html/template`, whose reflection would cost the binary megabytes.

Metrics: `kudurru_blockpage_requests_total{scheme}` (http, https),
`kudurru_blockpage_rate_limited_total{scheme}` and
`kudurru_blockpage_unblock_requests_total{result}` (sent, limited, paused,
invalid, error, cross_origin).

## Unblock requests

The page offers a **Request access** form under the explanation, so a user can
ask for the name instead of finding the administrator, and administrators get
one place, the dashboard's **Requests** page, to fix what blocks people.

### User flow

1. The user opens a blocked name and lands on the block page.
2. They may give a name and a way to reach them (both optional) and a short
   reason (at most 500 characters, plain text), and press **Request access**.
   The form is a plain HTML POST to `/.kudurru/request` on the same host;
   the page answers with a redirect back to itself (no script).
3. The page then shows the request's state for that device and name, on every
   later visit too:
   - **pending**: when it was sent; sending the form again updates the reason
     of the same request instead of filing another;
   - **approved**: no form; "try again in a moment", with how long the device
     may still remember the blocked answer (the block TTL counted from the
     decision; the browser or the operating system cache the answer, not
     kudurru);
   - **denied**: the administrator's note, and the form to ask again;
   - **ended** (a time-limited approval ran out, or nobody decided in time):
     the form to ask again.

The blocked name is always the Host header (or TLS server name) of the
request, never a form field, so a request can only be filed for the name the
browser was sent to the block page for.

### Administrator flow

The dashboard's **Requests** page (`/requests`, under Filtering, with a badge
counting the pending requests; the Dashboard shows a notice while some wait)
lists the requests newest first: the name, the requester (name and contact as
typed, the client and its group), when, the reason, and why the name is
blocked (list and rule, blocked service, safe search or allow-only policy,
with links to the list and policy pages). The detail page
(`/requests/<namespace>/<name>`) adds the client's recent queries for the name
(from the query log of the node that took the request) and the decision
history.

- **Approve** adds an Allowlist entry ([allowlist.md](allowlist.md); origin
  `unblock`, with the request, the approver and the note as comment) and
  attaches its list to the policies concerned:
  - *everyone*: the everyone list `ui-allow`, attached to every policy of
    the namespace kudurru may edit;
  - *group*: `ui-allow-<group>` in the group's namespace, created on demand
    (managed by the API) and attached to every policy whose `clientGroupRefs`
    names the group; refused when no policy covers the group.

  *exact* allows the name only, *subdomains* the name and every name below it
  (a wildcard entry `*.name`). A duration (1 h, 8 h, a day, a week, any Go duration up to a
  year) makes the approval time-limited; empty is permanent. Policies managed
  by a file or an integration are left alone and listed in the answer as
  skipped. There is no per-client scope: policies apply to ClientGroups, and a
  group of one client would take that client out of its own group's policy.
- **Deny** records the decision and the note shown to the user; denying an
  approval in force revokes it (its entry is removed).
- **Delete** removes the request. A permanent approval's entry stays on its
  list; a time-limited approval in force must be denied first, so its entry
  does not outlive its link.

Every decision writes an audit event (`unblockrequests.approve`, `.deny`,
`.delete`) and flushes the name from the node's response cache. The list write
makes every node recompile its lists, which also flushes the local edge
(dnsdist) cache; clients and downstream resolvers keep the block answer until
its TTL ends, which is what the approved page tells the user.

The leader removes the entry of a time-limited approval when it ends, only if
that approval added it (an entry that was on the list before stays) and no
other approval in force needs the same entry (that approval then inherits it).
The link from request to entry is in the request's `status.allow`, so removal
survives restarts and leader changes.

API: `GET /api/v1/unblockrequests?state=&limit=&continue=`,
`GET /api/v1/unblockrequests/{namespace}/{name}`,
`POST …/{namespace}/{name}/approve` (`{scope, match, group?, duration?, note?}`),
`POST …/deny` (`{note?}`) and `DELETE …/{namespace}/{name}`.

### Storage

Requests are `UnblockRequest` objects (`kudurru.io/v1alpha1`, namespaced, in
`default`): CRDs in Kubernetes (`kubectl get unblockrequests`, short name
`ubr`), rows of state.db with the sqlite store, so every node and every
administrator sees the same requests. The spec is what the block page
recorded; the status holds the state (`Pending`, `Approved`, `Denied`,
`Expired`), `decidedBy`, `decidedAt`, the note, the allow entry and the earlier
decisions of a request asked again. One object exists per recorded client and
name. Its name (`ur-<hex>`) is an HMAC of the two under a key derived from the
encryption key, because allow-list entries show it as their request to anyone
who reads lists; after an encryption key rotation, a client's next request for
the same name is a new object. The serving snapshot does not include them, so a request never rebuilds
it. Install the new CRD (the `kudurru-crds` chart, or `kubectl apply -f
crds.yaml`) before upgrading the server.

### Privacy

The client is recorded as `querylog.privacy` allows: the address under `full`
and `hide_domains`; the /24 or /48 network address under `anonymize_client`
with `anonymize: prefix` (the Requests page says the client is anonymised, and
devices of the same network share one request per name); nothing under
`anonymize: pseudonym`, `hide_domains_clients` and `nothing` ("hidden"; every
anonymous device shares one request per name). The client's groups and
policies are recorded in every mode. The name, contact and reason are what the
user chose to type.

### Limits

| Limit | Default | Key |
|---|---|---|
| requests per client (IPv6 per /64) per hour | 5 | `unblockRequests.rateLimit` |
| pending requests cluster-wide; beyond it the page says requests are paused | 1000 | `unblockRequests.maxPending` |
| requests one node stores per minute | 30 | fixed |
| form body | 8 KiB | fixed |
| name, contact, reason | 100, 254, 500 characters | fixed |
| pending requests expire after | 14 days | `unblockRequests.pendingExpiry` |
| decided and expired requests are deleted after | 30 days | `unblockRequests.retention` |

A repeated request of the same client for the same name updates the pending
one. A hidden honeypot field catches form-filling bots: such posts look
accepted and store nothing. Cross-site posts are refused: when the browser
sends `Origin` or `Sec-Fetch-Site`, it must be the block page's own origin. The
block page sets no cookies and asks for no credentials; the per-client and
per-node rate limits and the cap bound what an unauthenticated page can write
to the apiserver or state.db.

### Kubernetes RBAC

The server's ClusterRole may create, patch and delete `unblockrequests` and
update their status. For people: the resource `unblockrequests` has the verbs
`read`, `decide` and `delete`; `auditor` reads, `operator` (aggregated into
`edit`) reads, decides and deletes, `admin` has all. `viewer` (aggregated into
`view`) does not see them: requests carry client addresses and free text, like
the query log. A Kubernetes identity needs `get`/`list` on
`unblockrequests.kudurru.io` to read and the custom verb `decide` to approve or
deny (`kubectl auth can-i decide unblockrequests.kudurru.io`).

## HTTPS: why browsers warn

Most sites are opened over HTTPS. When a browser connects to the block page
address for `https://www.ads.example/`, the block page would have to present a
certificate valid for `www.ads.example`, which only that site's owner can get.
So:

- Without `listeners.blockPage.https`, the HTTPS connection is refused and the
  browser shows its own "cannot connect" error. Plain-HTTP visits (typed
  names, old links, captive-portal probes) get the page.
- With `listeners.blockPage.https`, the block page presents its own
  certificate (self-signed, or the configured one) and the browser shows a
  certificate warning. Only after the user clicks through does the page
  appear. Do not install the self-signed certificate as a trusted CA on
  clients to silence this: whoever holds its key could then impersonate any
  site to those clients.
- Sites on the HSTS preload list (google.com, facebook.com, most large sites)
  and sites the browser has seen with HSTS do not offer a click-through at
  all: the browser shows a hard error. The block page never sends
  `Strict-Transport-Security` itself, as it would pin the blocked name to HTTPS
  in the browser.
- Browsers that use their own DNS-over-HTTPS resolver bypass kudurru and never
  see the block answer (point them at kudurru's DoH or disable it by policy).
- `blockPage` answers HTTPS/SVCB queries with NODATA, so browsers fall back to
  A/AAAA instead of using an HTTPS record's hints.

The page is therefore an explanation for the cases a browser can show it, not a
guarantee: apps and HTTPS-only sites show their own errors.

## dnsdist edge and caching

- The block page listener runs on the kudurru node, not on dnsdist. Behind the
  edge (compose single-edge and HA, the chart's `edge.enabled`) set
  `blockPage.ipv4` to an address that reaches a kudurru node: the node's own
  address, or a VIP whose holder also runs kudurru (keepalived moves the VIP
  between hosts that each run dnsdist and kudurru in compose HA), or the chart's
  block page Service (it selects the kudurru pods, not the edge pods). The
  derived default skips loopback when an edge is configured, so set the
  address explicitly.
- The block answer is cached like any other: by dnsdist's packet cache, by
  clients and by downstream resolvers, for the block TTL (10 m by default).
  kudurru's own response cache never holds block answers (blocking runs before
  it). kudurru flushes the local edge's resolver cache when the lists change,
  when blocking is toggled and when the block answers change
  (`blocking.defaultResponse`, `blockTTL`, `customIP`, `blockPage.ipv4/ipv6`);
  clients and downstream resolvers keep the old answer until its TTL ends, so
  lower `blocking.blockTTL` before changing the block page address.
- In a cluster each node serves its own block page; the reason lookup reads
  that node's query log, so a client whose DNS query went to another node (or
  through the edge to another backend) sees the policy-evaluated reason, or
  none.

## Settings

| Key | Default | Notes |
|---|---|---|
| `blockPage.enabled` | `false` | restart required |
| `blockPage.ipv4`, `blockPage.ipv6` | (derived) | the addresses A/AAAA answers carry |
| `blockPage.title`, `blockPage.message` | see the settings reference | plain text |
| `blockPage.contact` | (none) | email address or http(s) URL |
| `blockPage.showDetails` | `true` | list, rule, service, policy, dashboard link |
| `blockPage.dashboardURL` | (derived) | base URL of the dashboard |
| `blockPage.rateLimit` | `120` | per client per minute; 0 disables |
| `unblockRequests.enabled` | `true` | the Request access form |
| `unblockRequests.rateLimit` | `5` | requests per client per hour |
| `unblockRequests.maxPending` | `1000` | cluster-wide cap |
| `unblockRequests.pendingExpiry` | `336h` | undecided requests expire |
| `unblockRequests.retention` | `720h` | decided requests are deleted |
| `listeners.blockPage.http` | `:80` | host only |
| `listeners.blockPage.https` | (off) | host only |
| `listeners.blockPage.tls.certFile`, `keyFile` | (self-signed) | host only |

The full reference is `kudurru config schema` and the generated settings
reference of the docs site.
