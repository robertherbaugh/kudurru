# Blocked services and safe search

A Policy can block whole services by name (`blockedServices: [youtube,
tiktok]`), whole categories of services (`blockedCategories: [ai]`), and
force the search engines' safe-search mode (`safeSearch: true`). All of
them read the kudurru blocked-services and safe-search catalogue (decision
D60): Apache-2.0 data written for kudurru from each vendor's own
documentation and the services' first-party domains. The catalogue is
built into the binary (`internal/listdata/catalogue/catalogue.json`,
sources per entry in `PROVENANCE.md` next to it) and can be replaced at
runtime from a URL. An administrator can also opt in to AdGuard's
HostlistsRegistry services (GPL-3.0), which the instance then downloads
itself; kudurru does not ship them (see below).

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
  blockedCategories: [ai, gambling]   # every service in them, also ones added later
  allowedServices: [huggingface]      # exceptions inside a blocked category
  safeSearch: true
  safeSearchMode: moderate   # optional; YouTube only
```

The dashboard's Policy page and the policy wizard's What step show the
catalogue as a logo grid: one tab per category with counts ("AI · 19 · 3
blocked"), a search over names and domains, a "Blocked only" filter, one
section per category with a "Block the whole category" switch, and the
SafeSearch switch in its own section. `GET /api/v1/catalogue/services` lists
the service IDs, `GET /api/v1/catalogue/categories` the categories,
`GET /api/v1/catalogue/safesearch` the engines, and `GET /api/v1/catalogue`
the version in service, where it came from and which sources are active
with their licences. All four need `policies.read`.

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
9GAG, VK, Vimeo, Tinder, Pixabay, Minecraft, most dating, gambling, news
and crypto brands) has no `icon`; the dashboard draws a monogram.

### Categories

| ID | Name | Services in the embedded catalogue |
|---|---|---|
| `ai` | AI | ChatGPT (with the OpenAI API and Sora), Claude (with the Anthropic API), Google Gemini and AI Studio, Microsoft Copilot, Perplexity, Mistral Vibe (Le Chat), DeepSeek, Grok, Meta AI, Character.AI, Poe, Hugging Face, Midjourney, Stability AI, Runway, Suno, ElevenLabs, Replicate, Pi |
| `social` | Social media | TikTok, Facebook, Instagram, Threads, Snapchat, X (Twitter), Reddit, Pinterest, Tumblr, LinkedIn, 9GAG, VK, Bluesky, BeReal, Mastodon (mastodon.social), Weibo, OK, Imgur, Lemon8 |
| `messaging` | Messaging | WhatsApp, Discord, Telegram, Signal, Viber, WeChat, LINE, KakaoTalk |
| `video` | Video and streaming | YouTube, Twitch, Netflix, Prime Video, Disney+, Hulu, Vimeo, HBO Max, Paramount+, Peacock, Apple TV+, Crunchyroll, Plex, Kick, Dailymotion, Pluto TV, Tubi, Bilibili |
| `music` | Music and audio | Spotify, SoundCloud, Deezer, TIDAL, Pandora, Apple Music, YouTube Music |
| `gaming` | Gaming | Steam, Epic Games, Fortnite, Roblox, Minecraft, Riot Games, VALORANT, Xbox network, PlayStation Network, Nintendo Switch Online, Battle.net, EA app, Ubisoft Connect, GOG, itch.io, HoYoverse, Supercell, Rockstar Games, PUBG |
| `shopping` | Shopping | Amazon, eBay, Temu, SHEIN, AliExpress, Etsy, Walmart |
| `dating` | Dating | Tinder, Bumble, Hinge, OkCupid, Grindr, Match, Badoo, Plenty of Fish |
| `gambling` | Gambling and betting | bet365, DraftKings, FanDuel, William Hill, Betfair, PokerStars, Stake, Betway |
| `news` | News | CNN, BBC, The New York Times, Fox News, The Guardian |
| `forums` | Forums and Q&A | Quora, 4chan, Stack Overflow and Stack Exchange |
| `productivity` | Productivity and cloud storage | Dropbox, Google Drive, OneDrive, iCloud, Box, MEGA, WeTransfer, Notion, Slack, Zoom, MediaFire |
| `vpn` | VPN and proxy | NordVPN, ExpressVPN, Proton VPN, Surfshark, Private Internet Access, CyberGhost, Windscribe, TunnelBear, Hotspot Shield, Psiphon, Tor Project |
| `crypto` | Cryptocurrency exchanges | Binance, Coinbase, Kraken, Crypto.com, Bybit, OKX, KuCoin |
| `other` | Other | none; imported services without a matching category |

Adult sites have no service entries: no vendor-neutral source lists them.
Use the Adult content category of the Block List Project instead (content
categories, below).

### Blocked services and categories

- Each service compiles like a block list named `service:<id>`, with
  adblock rules (`||youtube.com^`, which blocks the domain and its
  subdomains). The policy's block response and block TTL apply.
- `blockedCategories` blocks every service the catalogue in service has in
  those categories, as if each were listed in `blockedServices`. A service
  a catalogue update adds to a blocked category is blocked from the next
  snapshot on, without editing the policy.
- `allowedServices` are exceptions to `blockedCategories` only: a service
  listed there and in a blocked category is not blocked by this policy. It
  does not cancel an ID in `blockedServices`, nor a block of another
  policy that applies to the same client. The dashboard writes it when a
  service tile under a blocked category is clicked.
- `GET /api/v1/policies/{ns}/{name}/state` reports `services`
  (blockedServices), `categories` (with their service counts and
  exceptions), `allowedServices` and `effectiveServices`: what the policy
  blocks now, each marked `via: service` or `via: category`.
- The query log shows status `blocked_list`, list `service:youtube` and the
  rule that matched. `GET /blocklists/_search?domain=...` reports service
  lists too.
- Allow lists still win. Put `music.youtube.com` on an allow list of the
  same (or any applied) policy and it resolves while the rest of YouTube is
  blocked.
- The response-side checks apply as for any list: a CNAME into a blocked
  service's domain is blocked (`blocked_cname`).
- An ID the catalogue does not have is skipped. The Policy reports
  `Degraded` with reason `UnknownService` (an unknown category:
  `UnknownCategory`), and `GET /info/messages` has a warning. IDs are not
  an enum in the CRD because the catalogue can change at runtime; the
  condition clears when a catalogue update adds the ID.
- Only first-party domains are listed. Shared infrastructure stays
  reachable: Microsoft, Google and Apple sign-in, generic CDNs and cloud
  storage, Amazon retail for Prime Video. Where blocking one service needs
  another, the entry says so in `requires` and the grid offers to block it
  too: Fortnite requires Epic Games, VALORANT requires Riot Games. `note`
  carries caveats such as "VPN apps connect to their servers by IP address:
  this blocks the website, sign-up and app updates, not an established
  tunnel", and `related` names services of the same vendor.

### Content categories

The Block List Project ([blocklistproject/Lists](https://github.com/blocklistproject/Lists))
publishes one large list per topic. The list wizard offers them as
well-known lists, and the Policy page and the policy wizard as "content
categories":

| Content category | List (one domain per line) |
|---|---|
| Gambling, Adult content, Drugs, Vaping, Piracy, Torrent sites | `gambling`, `porn`, `drugs`, `vaping`, `piracy`, `torrent` |
| Malware, Phishing, Scams, Fraud, Ransomware, Cryptojacking, Malicious redirects | `malware`, `phishing`, `scam`, `fraud`, `ransomware`, `crypto`, `redirect` |
| Ads, Tracking, Smart TV telemetry | `ads`, `tracking`, `smart-tv` |

Turning one on creates, if it does not exist yet, the BlockList
`blp-<id>` in the policy's namespace with one URL source,
`https://blocklistproject.github.io/Lists/alt-version/<list>-nl.txt`
(format `domains`), and adds it to the policy's `blockListRefs`. Nothing
is embedded: the list is downloaded and refreshed like any URL list
(`blocking.refreshInterval`). Turning it off removes the reference and
keeps the BlockList for other policies. The malware list has about 2.6
million names and needs several hundred MB of memory.

Licence, as checked at commit `ee3bdbaddf49c756cf3340bb2e6a9dee5282d1c6`
(2026-07-20): the repository's `LICENSE` is the Unlicense (public domain)
and the list headers say MIT; several lists merge third-party upstream
lists (`config/lists.yml`), the gambling and porn lists among them HaGeZi's
(GPL-3.0). kudurru names the URLs only; each instance downloads the lists.

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
`schemaVersion` this build does not read, a malformed document, an HTTP
error or a network error keeps the catalogue in service: the last good
download, else the embedded one. `GET /api/v1/catalogue` reports `origin`, `version`,
`digest` and `refresh.lastError`.

### Document format (schema version 2)

```json
{
  "schemaVersion": 2,
  "version": "2026.10.12",
  "license": "Apache-2.0",
  "homepage": "https://...",
  "categories": [
    {
      "id": "gaming",
      "name": "Gaming",
      "description": "Game stores, console networks and online games.",
      "icon": "gamepad-2",
      "order": 60
    }
  ],
  "services": [
    {
      "id": "fortnite",
      "name": "Fortnite",
      "category": "gaming",
      "rules": ["||fortnite.com^"],
      "source": "first-party domains of the service",
      "requires": ["epicgames"],
      "note": "Fortnite signs in and updates through Epic Games, so block Epic Games as well to stop it."
    },
    {
      "id": "epicgames",
      "icon": "epic-games",
      "name": "Epic Games",
      "category": "gaming",
      "rules": ["||epicgames.com^", "||epicgames.dev^"],
      "source": "first-party domains of the service",
      "related": ["fortnite"]
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

- `schemaVersion` is `2` or `1`. Version 1 documents (no `categories`, no
  hints) are still read: their services' category IDs get the names,
  descriptions and icons of the embedded catalogue's categories, and a
  category the embedded one lacks gets a name made from its ID ("retro-games"
  becomes "Retro games") and sorts last; `requires`, `related` and `note`
  are ignored in them. Any other version is refused, never half-read.
  Fields this build does not know are ignored, so a version may grow
  additively.
- `version` and `license` are non-empty. `version` is the date,
  `YYYY.MM.DD`.
- Category, service and engine `id`s match
  `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` and are unique within their kind;
  `name` and `source` are non-empty (at most 256 bytes).
- Categories (version 2): `description` is optional (at most 256 bytes);
  `icon` is optional and names a [Lucide](https://lucide.dev) icon in
  kebab-case (`gamepad-2`); the dashboard draws a tag for a name it does
  not bundle. `order` sorts them (ascending, then by ID). At most 100.
- A service's `category` names one of the document's categories (version
  2).
- `icon` is optional on services and engines and matches the ID pattern:
  the slug of a logo built into the binary (`GET /api/v1/catalogue/icons`),
  for when it differs from the `id`. Without it an entry uses the logo named
  like its `id`, if there is one. A slug this build does not have is not an
  error: the entry has no logo.
- `requires` and `related` (version 2, optional, at most 20 each) name
  other services of the same document; `note` is optional, at most 256
  bytes.
- A service has 1 to 1000 `rules`, each a single adblock rule that compiles
  to a block entry: no `@@` exceptions, no modifiers, no cosmetic rules, no
  comments.
- An engine has either `target` (with an optional `moderateTarget`) or
  `addresses` (unicast IPv4 or IPv6), and at least one `domain`. Host names
  are lower-case A-labels without the trailing dot. A domain is rewritten
  by one engine only and is never its own target.
- Limits: 4 MiB per document, 1000 services, 100 categories, 100 engines,
  10,000 safe-search domains.

### URL contract for the data repository

D60 puts the catalogue in a separate public data repository owned by the
project. kudurru expects, at a stable URL such as
`https://raw.githubusercontent.com/<owner>/<repo>/main/v2/catalogue.json`:

- one document per schema version, under a path that names the version
  (`v1/catalogue.json`, `v2/catalogue.json`), so a release that reads
  version 3 can move to `v3/` while older releases keep reading theirs;
  this build reads both `v1/` and `v2/` documents;
- `Content-Type: application/json`, an `ETag` or `Last-Modified` header for
  conditional GETs, and HTTPS;
- every entry with its `source`, a permissive licence (Apache-2.0 or CC0)
  in `license`, and a provenance file in the repository. Nothing derived
  from GPL tables (AdGuard's blocked-services and safe-search lists) or any
  other copyleft source.

Until that repository exists, leave `blocking.catalogue.url` empty: the
embedded catalogue is complete, and each release ships the catalogue
current at its build.

## AdGuard's services (opt-in, GPL-3.0)

| Setting | Default | |
|---|---|---|
| `blocking.catalogue.adguardServicesURL` | empty (off) | URL of AdGuard's HostlistsRegistry blocked-services file, e.g. `https://raw.githubusercontent.com/AdguardTeam/HostlistsRegistry/main/assets/services.json` |

AdGuard's HostlistsRegistry lists about 140 services. Its data is GPL-3.0,
so kudurru neither ships, vendors, pre-fetches nor derives from it: the
default is off, nothing of it is in the binary, the images or the tests
(they use a hand-written document in the same format with example.com
domains), and kudurru's own catalogue is written without consulting it.
Setting the URL makes your instance download it, the same way it downloads
a block list you configure. Whether the GPL's terms matter to you depends
on how you use and share the data; kudurru does not distribute it.

What the opt-in does:

- The leader downloads the file on `blocking.catalogue.refreshInterval`
  (conditional GET, through the guarded list client; loopback and
  link-local URLs need `blocking.allowLocalListURLs`). Replicas pull the
  leader's copy over the internal listener
  (`GET /internal/catalogue/adguard`, sha256-verified) and download it
  themselves only after the leader has been unreachable for 10 minutes.
  The last good download is kept in
  `<node.dataDir>/catalogue/adguard-services.json` and served at the next
  start; a failed or empty download keeps the previous one. The setting
  needs a restart.
- The file's format: an object with `blocked_services`, each
  `{id, name, rules, group}` (a bare array is read too, `icon_svg` is
  ignored and the dashboard draws a monogram), at most 16 MiB. IDs become
  kudurru IDs with `_` replaced by `-` (`amazon_streaming` becomes
  `amazon-streaming`). Rules that are not plain block rules are dropped,
  and entries without a usable rule, name or ID are skipped
  (`GET /api/v1/catalogue` counts both).
- Imported services join the catalogue next to kudurru's own, with
  `origin: adguard`, `license: GPL-3.0` and
  `source: adguard-hostlists-registry`; the grid marks them
  "AdGuard · GPL-3.0". On an ID both have, kudurru's entry wins and the
  imported one is left out (counted as `shadowed`), so turning the opt-in
  on never changes what an existing policy's `blockedServices` block.
  Policies name imported services by ID like any other; turning the opt-in
  off makes those IDs `UnknownService`.
- AdGuard's groups map to categories: `ai` to `ai`, `dating`, `gambling`,
  `gaming` and `shopping` to the same IDs, `messenger` to `messaging`,
  `social_network` to `social`, `streaming` to `video`; `cdn`, `hosting`,
  `privacy`, `software` and anything else to `other`. A blocked category
  covers the imported services in it too.
- `GET /api/v1/catalogue` lists the active `sources`: `kudurru`
  (Apache-2.0, embedded or `blocking.catalogue.url`) and, when configured,
  `adguard` (GPL-3.0) with its URL, digest, service count, shadowed and
  skipped entries and refresh state. The Settings page shows the same
  under the setting.
