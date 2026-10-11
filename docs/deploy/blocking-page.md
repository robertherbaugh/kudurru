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
  link.

The reason is the most recent block of that name for that client within ten
minutes: first the query log's in-memory tail (which has the query even before
the database flush), then `querylog.db`. The query-log privacy mode limits the
lookup: `full` matches client and name; `anonymize_client` matches the client's
/24 or /48 (prefix) or the name alone (pseudonym); `hide_domains` and stricter
modes, and a disabled query log, keep no name to match. Without a query-log
row, the name is evaluated against the policy that applies to the client
address now (what the blocking stage does for the question; response-side
blocks are not visible that way). Without either, the page says why no reason
is shown.

`/favicon.ico`, `/robots.txt` (`Disallow: /`) and `/health` (`ok`) are served
too. GET and HEAD answer 200, every other method 405. Each client address
(IPv6 per /64) gets `blockPage.rateLimit` requests per minute (default 120,
0 disables it); beyond it the page answers 429 with `Retry-After`. `/health`
is never limited. The listener never proxies or fetches anything.

The page is self-contained: the logo is inlined, the style sheet is allowed by
its hash, nothing loads from elsewhere and nothing runs:
`Content-Security-Policy: default-src 'none'; style-src 'sha256-...';
img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors
'none'`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy:
no-referrer`. Every value (Host, settings, list names, rules) is escaped by
`html/template`.

Metrics: `kudurru_blockpage_requests_total{scheme}` (http, https) and
`kudurru_blockpage_rate_limited_total{scheme}`.

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
| `listeners.blockPage.http` | `:80` | host only |
| `listeners.blockPage.https` | (off) | host only |
| `listeners.blockPage.tls.certFile`, `keyFile` | (self-signed) | host only |

The full reference is `kudurru config schema` and the generated settings
reference of the docs site.
