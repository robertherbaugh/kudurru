# Blocked services and safe search

A Policy can block whole services by name (`blockedServices: [youtube,
tiktok]`) and force the search engines' safe-search mode
(`safeSearch: true`). Both read the kudurru blocked-services and
safe-search catalogue (decision D60): Apache-2.0 data written for kudurru
from each vendor's own documentation and the services' first-party
domains. The catalogue is built into the binary
(`internal/listdata/catalogue/catalogue.json`, sources per entry in
`PROVENANCE.md` next to it) and can be replaced at runtime from a URL.

## Using it

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata:
  name: kids
  namespace: default
spec:
  clientGroupRefs: [kids]
  priority: 10
  blockedServices: [youtube, tiktok, roblox]
  safeSearch: true
  safeSearchMode: moderate   # optional; YouTube only
```

The dashboard's Policy form has a "Blocked services" picker and the
SafeSearch switch. `GET /api/v1/catalogue/services` lists the service IDs,
`GET /api/v1/catalogue/safesearch` the engines, and `GET /api/v1/catalogue`
the version in service and where it came from. All three need
`policies.read`.

Services and engines carry `icon` (and `iconOnDark` for dark artwork), the
path of a logo built into the binary: `GET /api/v1/catalogue/icons/{slug}.svg`
(`image/svg+xml`, strong ETag, `Cache-Control: public, max-age=86400`).
`GET /api/v1/catalogue/icons` lists every logo, including the upstream DNS
providers the Upstreams page shows (cloudflare, google, adguard-home,
nextdns, mullvad, cisco for OpenDNS, control-d). Both routes are public:
the logos are static files of the build. They come from
[homarr-labs/dashboard-icons](https://github.com/homarr-labs/dashboard-icons)
(Apache-2.0; commit, files and checksums in
`internal/listdata/catalogue/icons/PROVENANCE.md`), carry no script, event
handler or external reference, and are trademarks of their owners, shown
only to identify the service. An entry without a logo (Roblox, Fortnite,
9GAG, VK, Vimeo, Tinder, Pixabay, Minecraft) has no `icon`; the dashboard
draws a monogram.

### Blocked services

- Each service compiles like a block list named `service:<id>`, with
  adblock rules (`||youtube.com^`, which blocks the domain and its
  subdomains). The policy's block response and block TTL apply.
- The query log shows status `blocked_list`, list `service:youtube` and the
  rule that matched. `GET /blocklists/_search?domain=...` reports service
  lists too.
- Allow lists still win. Put `music.youtube.com` on an allow list of the
  same (or any applied) policy and it resolves while the rest of YouTube is
  blocked.
- The response-side checks apply as for any list: a CNAME into a blocked
  service's domain is blocked (`blocked_cname`).
- An ID the catalogue does not have is skipped. The Policy reports
  `Degraded` with reason `UnknownService`, and `GET /info/messages` has a
  warning. IDs are not an enum in the CRD because the catalogue can change
  at runtime; the condition clears when a catalogue update adds the ID.
- Only first-party domains are listed. Shared infrastructure stays
  reachable: Microsoft and Xbox sign-in for Minecraft, generic CDNs, Amazon
  retail for Prime Video. Fortnite signs in through Epic Games, so block
  `epicgames` as well to stop the game.

### Safe search

With `safeSearch: true` the blocking stage answers the engines' host
names with the vendor's documented safe target:

| Engine | Host names | Answer |
|---|---|---|
| Google | `www.google.<tld>` for every domain on Google's published supported-domains list | CNAME `forcesafesearch.google.com` |
| YouTube | `www.youtube.com`, `m.youtube.com`, `youtubei.googleapis.com`, `youtube.googleapis.com`, `www.youtube-nocookie.com` | CNAME `restrict.youtube.com` (strict) or `restrictmoderate.youtube.com` (`safeSearchMode: moderate`) |
| Bing | `www.bing.com`, `edgeservices.bing.com` | CNAME `strict.bing.com` |
| DuckDuckGo | `duckduckgo.com` | CNAME `safe.duckduckgo.com` |
| Brave Search | `search.brave.com` | CNAME `forcesafe.search.brave.com` |
| Yandex | `yandex.ru` | A `213.180.193.56` (Yandex documents an address, not a host name) |
| Pixabay | `pixabay.com` | CNAME `safesearch.pixabay.com` |

- The answer is the CNAME followed by the target's own records of the
  question type, resolved through the cache and the client's upstream
  group (the policy's `upstreamGroup` applies). A CNAME question gets the
  CNAME alone. The synthesised records carry the block TTL, so turning
  safe search off takes effect as quickly as an unblock.
- The query log shows status `blocked_safesearch` (class `blocked`), list
  `safesearch:<engine>` and rule `CNAME <target>`. With EDNS the answer
  carries EDE 4 (Forged Answer) naming the engine. The metric is
  `kudurru_block_decisions_total{action="rewrite",source="safesearch"}`.
- An allow list match for the name skips safe search, as it skips every
  block.
- `safeSearchMode` only changes YouTube; every other engine has one level.
- Like the block lists, safe search follows `blocking.enabled` and the
  temporary disable (`POST /dns/blocking`): when blocking is off, nothing is
  rewritten.
- Only the documented host names are rewritten. Browsers that resolve
  search through DNS over HTTPS to another resolver, or a VPN, bypass it, as
  they bypass any DNS filter. Block the encrypted resolvers with a block
  list when that matters.

## Updating the catalogue at runtime

| Setting | Default | |
|---|---|---|
| `blocking.catalogue.url` | empty | URL of a catalogue document; empty serves the embedded one |
| `blocking.catalogue.refreshInterval` | `24h` | how often the leader downloads it (at least `1m`) |

Both need a restart. The leader downloads the URL with a conditional GET
(`If-None-Match`, `If-Modified-Since`) through the same guarded HTTP client
as BlockList URLs: loopback, link-local and metadata addresses need
`blocking.allowLocalListURLs`. Replicas pull the leader's copy over the
internal listener (`GET /internal/catalogue`, sha256-verified) every 5
minutes and download the URL themselves only after the leader has been
unreachable for 10 minutes. The last good download is kept in
`<node.dataDir>/catalogue/` and served at the next start.

A document is put in service only after it parses and validates. A
different `schemaVersion`, a malformed document, an HTTP error or a
network error keeps the catalogue in service: the last good download, else
the embedded one. `GET /api/v1/catalogue` reports `origin`, `version`,
`digest` and `refresh.lastError`.

### Document format (schema version 1)

```json
{
  "schemaVersion": 1,
  "version": "2026.10.11",
  "license": "Apache-2.0",
  "homepage": "https://...",
  "services": [
    {
      "id": "twitter",
      "icon": "x",
      "name": "X (Twitter)",
      "category": "social",
      "rules": ["||x.com^", "||twitter.com^"],
      "source": "first-party domains of the service"
    }
  ],
  "safeSearch": [
    {
      "id": "youtube",
      "name": "YouTube",
      "target": "restrict.youtube.com",
      "moderateTarget": "restrictmoderate.youtube.com",
      "domains": ["www.youtube.com", "m.youtube.com"],
      "source": "https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users"
    },
    {
      "id": "yandex",
      "name": "Yandex",
      "addresses": ["213.180.193.56"],
      "domains": ["yandex.ru"],
      "source": "https://yandex.com/support/search/schoolsearch.html"
    }
  ]
}
```

Rules this build checks (`catalogue.Parse`):

- `schemaVersion` must be `1`. A build that reads a newer schema ships a new
  embedded catalogue; documents of another version are refused, never
  half-read. Fields this build does not know are ignored, so version 1 may
  grow additively.
- `version` and `license` are non-empty. `version` is the date,
  `YYYY.MM.DD`.
- Service and engine `id`s match `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
  and are unique; `category` matches the same pattern; `name` and `source`
  are non-empty (at most 256 bytes).
- `icon` is optional on services and engines and matches the same pattern:
  the slug of a logo built into the binary (`GET /api/v1/catalogue/icons`),
  for when it differs from the `id`. Without it an entry uses the logo named
  like its `id`, if there is one. A slug this build does not have is not an
  error: the entry has no logo.
- A service has 1 to 1000 `rules`, each a single adblock rule that compiles
  to a block entry: no `@@` exceptions, no modifiers, no cosmetic rules, no
  comments.
- An engine has either `target` (with an optional `moderateTarget`) or
  `addresses` (unicast IPv4 or IPv6), and at least one `domain`. Host names
  are lower-case A-labels without the trailing dot. A domain is rewritten
  by one engine only and is never its own target.
- Limits: 4 MiB per document, 1000 services, 100 engines, 10,000
  safe-search domains.

### URL contract for the data repository

D60 puts the catalogue in a separate public data repository owned by the
project. kudurru expects, at a stable URL such as
`https://raw.githubusercontent.com/<owner>/<repo>/main/v1/catalogue.json`:

- one document per schema version, under a path that names the version
  (`v1/catalogue.json`), so a release that reads version 2 can move to
  `v2/` while older releases keep reading `v1/`;
- `Content-Type: application/json`, an `ETag` or `Last-Modified` header for
  conditional GETs, and HTTPS;
- every entry with its `source`, a permissive licence (Apache-2.0 or CC0)
  in `license`, and a provenance file in the repository. Nothing derived
  from GPL tables (AdGuard's blocked-services and safe-search lists) or any
  other copyleft source.

Until that repository exists, leave `blocking.catalogue.url` empty: the
embedded catalogue is complete for v0.1, and each release ships the
catalogue current at its build.
