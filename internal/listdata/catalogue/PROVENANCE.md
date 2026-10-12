# Provenance: blocked-services and safe-search catalogue

SPDX-License-Identifier: Apache-2.0

| Field | Value |
|---|---|
| File | `catalogue.json` (this directory), embedded into the kudurru binary by `internal/listdata/catalogue` (`go:embed`) |
| Purpose | `Policy.spec.blockedServices` and `Policy.spec.safeSearch` (docs/architecture.md §7, D60) |
| Schema version | 2 |
| Catalogue version | `2026.10.12` |
| SHA-256 | `a92265ef6c6b7688bca6fe2c2fd2902b457e1799bbd57efc82f23d251c5de1c3` |
| Size | 20,372 bytes, 15 categories, 32 services, 7 safe-search engines (25 services name a logo, `icon`: see `icons/PROVENANCE.md`) |
| Licence | Apache-2.0, Copyright 2026 Robert Herbaugh and the kudurru contributors (`LICENSE` in this directory, the repository's licence) |
| Author | kudurru; every entry written by hand for kudurru from the sources in the tables below |
| Not used | AdGuard Home's blocked-services and safe-search tables (GPL-3.0) and any other copyleft source: they were neither copied nor consulted |

Each entry is either taken from the vendor's own public documentation
(the URL is recorded) or lists the service's first-party domains: the
domains the vendor itself registers and serves the product from, which
are facts about the service rather than anyone's creative selection.
Shared infrastructure (generic CDNs, Microsoft and Google account
sign-in, Amazon retail) is deliberately left out so that blocking one
service does not break others.

The Google safe-search domains are generated, not hand-written: `www.`
followed by every entry of Google's published list of its search
domains, `https://www.google.com/supported_domains`, fetched on
2026-10-11 (187 entries, SHA-256 `ba827017007f54b698de62a5e3bb290327c12ddc0f9c88503511725c0baf3c49`), as
https://support.google.com/websearch/answer/186669 instructs ("www.google.com and any other Google country or region domains").

## Categories

kudurru-authored names, descriptions and sort order; the icons name
[Lucide](https://lucide.dev) icons (ISC), which the dashboard bundles.

| Kind | ID | Name | Order | Icon (Lucide) | Services |
|---|---|---|---|---|---|
| category | ai | AI | 10 | `sparkles` | 1 |
| category | social | Social media | 20 | `users` | 12 |
| category | messaging | Messaging | 30 | `message-circle` | 4 |
| category | video | Video and streaming | 40 | `tv` | 7 |
| category | music | Music and audio | 50 | `music` | 1 |
| category | gaming | Gaming | 60 | `gamepad-2` | 6 |
| category | shopping | Shopping | 70 | `shopping-cart` | 0 |
| category | dating | Dating | 80 | `heart` | 1 |
| category | gambling | Gambling and betting | 90 | `dices` | 0 |
| category | news | News | 100 | `newspaper` | 0 |
| category | forums | Forums and Q&A | 110 | `messages-square` | 0 |
| category | productivity | Productivity and cloud storage | 120 | `cloud` | 0 |
| category | vpn | VPN and proxy | 130 | `shield` | 0 |
| category | crypto | Cryptocurrency exchanges | 140 | `bitcoin` | 0 |
| category | other | Other | 1000 | `shapes` | 0 |

## Services

| Kind | ID | Name | Category | Rules | Source |
|---|---|---|---|---|---|
| service | 9gag | 9GAG | social | 2 | first-party domains of the service |
| service | chatgpt | ChatGPT | ai | 4 | first-party domains of the service |
| service | discord | Discord | messaging | 5 | first-party domains of the service |
| service | disneyplus | Disney+ | video | 5 | first-party domains of the service |
| service | epicgames | Epic Games | gaming | 2 | first-party domains of the service |
| service | facebook | Facebook | social | 8 | first-party domains of the service |
| service | fortnite | Fortnite | gaming | 2 | first-party domains of the service |
| service | hulu | Hulu | video | 3 | first-party domains of the service |
| service | instagram | Instagram | social | 4 | first-party domains of the service |
| service | linkedin | LinkedIn | social | 3 | first-party domains of the service |
| service | minecraft | Minecraft | gaming | 4 | first-party domains of the service; Microsoft account and Xbox sign-in are shared with other Microsoft services and not included |
| service | netflix | Netflix | video | 7 | first-party domains of the service |
| service | pinterest | Pinterest | social | 8 | first-party domains of the service |
| service | primevideo | Amazon Prime Video | video | 5 | first-party domains of the service |
| service | reddit | Reddit | social | 4 | first-party domains of the service |
| service | riotgames | Riot Games (League of Legends, Valorant) | gaming | 4 | first-party domains of the service |
| service | roblox | Roblox | gaming | 4 | first-party domains of the service |
| service | signal | Signal | messaging | 5 | first-party domains of the service |
| service | snapchat | Snapchat | social | 6 | first-party domains of the service |
| service | spotify | Spotify | music | 5 | first-party domains of the service |
| service | steam | Steam | gaming | 8 | first-party domains of the service |
| service | telegram | Telegram | messaging | 5 | first-party domains of the service |
| service | threads | Threads | social | 2 | first-party domains of the service |
| service | tiktok | TikTok | social | 10 | first-party domains of the service |
| service | tinder | Tinder | dating | 2 | first-party domains of the service |
| service | tumblr | Tumblr | social | 2 | first-party domains of the service |
| service | twitch | Twitch | video | 6 | first-party domains of the service |
| service | twitter | X (Twitter) | social | 4 | first-party domains of the service |
| service | vimeo | Vimeo | video | 2 | first-party domains of the service |
| service | vk | VK | social | 5 | first-party domains of the service |
| service | whatsapp | WhatsApp | messaging | 3 | first-party domains of the service |
| service | youtube | YouTube | video | 10 | first-party domains of the service; YouTube host names from https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users |

## Safe search

| Kind | ID | Name | Domains | Source |
|---|---|---|---|---|
| safesearch | bing | Bing | 2 | https://support.microsoft.com/en-us/topic/946059ed-992b-46a0-944a-28e8fb8f1814 |
| safesearch | brave | Brave Search | 1 | https://search.brave.com/help/safesearch |
| safesearch | duckduckgo | DuckDuckGo | 1 | https://duckduckgo.com/duckduckgo-help-pages/features/safe-search |
| safesearch | google | Google Search | 187 | https://support.google.com/websearch/answer/186669; domains: www. plus every entry of https://www.google.com/supported_domains |
| safesearch | pixabay | Pixabay | 1 | https://pixabay.com/blog/posts/id-140/ |
| safesearch | yandex | Yandex | 1 | https://yandex.com/support/search/schoolsearch.html |
| safesearch | youtube | YouTube | 5 | https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users |

Engine notes, from the cited pages:

- Google: CNAME `www.google.<tld>` to `forcesafesearch.google.com`.
- YouTube: CNAME the five host names Google lists to `restrict.youtube.com` (Strict Restricted Mode) or `restrictmoderate.youtube.com` (Moderate); Google warns against rewriting other YouTube names, so `youtube.com` and `youtu.be` are not rewritten.
- Bing: CNAME `www.bing.com` and `edgeservices.bing.com` (the Edge sidebar) to `strict.bing.com`.
- DuckDuckGo: map `duckduckgo.com` to `safe.duckduckgo.com`.
- Brave Search: `search.brave.com IN CNAME forcesafe.search.brave.com`.
- Yandex: the page publishes an address, not a host name: `213.180.193.56 yandex.ru` ("the Yandex IP address with the preset family search"), answered as an A record.
- Pixabay: CNAME `pixabay.com` to `safesearch.pixabay.com`.

## Updating

Edit `catalogue.json`, bump `version` (the date, `YYYY.MM.DD`), add or
update the entry's row above with its source, and record the new
SHA-256. `TestProvenance` in this package fails when the digest, the
licence line or any entry's row is out of date, and `TestEmbedded`
validates the document as the runtime refresher does. The separate
public data repository (D60) publishes the same file; its format and
URL contract are in docs/deploy/blocked-services.md.
