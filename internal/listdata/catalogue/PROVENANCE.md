# Provenance: blocked-services and safe-search catalogue

SPDX-License-Identifier: Apache-2.0

| Field | Value |
|---|---|
| File | `catalogue.json` (this directory), embedded into the kudurru binary by `internal/listdata/catalogue` (`go:embed`) |
| Purpose | `Policy.spec.blockedServices` and `Policy.spec.safeSearch` (docs/architecture.md §7, D60) |
| Schema version | 2 |
| Catalogue version | `2026.10.12` |
| SHA-256 | `575e421c03ccd4916ccb3ca295374be5050c0207e60edc9a68ea219a93a4b0a1` |
| Size | 66,497 bytes, 15 categories, 150 services, 7 safe-search engines (74 services name a logo, `icon`: see `icons/PROVENANCE.md`) |
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
| category | ai | AI | 10 | `sparkles` | 19 |
| category | social | Social media | 20 | `users` | 19 |
| category | messaging | Messaging | 30 | `message-circle` | 8 |
| category | video | Video and streaming | 40 | `tv` | 18 |
| category | music | Music and audio | 50 | `music` | 7 |
| category | gaming | Gaming | 60 | `gamepad-2` | 19 |
| category | shopping | Shopping | 70 | `shopping-cart` | 7 |
| category | dating | Dating | 80 | `heart` | 8 |
| category | gambling | Gambling and betting | 90 | `dices` | 8 |
| category | news | News | 100 | `newspaper` | 5 |
| category | forums | Forums and Q&A | 110 | `messages-square` | 3 |
| category | productivity | Productivity and cloud storage | 120 | `cloud` | 11 |
| category | vpn | VPN and proxy | 130 | `shield` | 11 |
| category | crypto | Cryptocurrency exchanges | 140 | `bitcoin` | 7 |
| category | other | Other | 1000 | `shapes` | 0 |

## Services

| Kind | ID | Name | Category | Rules | Source |
|---|---|---|---|---|---|
| service | 4chan | 4chan | forums | 3 | first-party domains of the service |
| service | 9gag | 9GAG | social | 2 | first-party domains of the service |
| service | aliexpress | AliExpress | shopping | 4 | first-party domains of the service |
| service | amazon | Amazon | shopping | 24 | first-party domains of the service |
| service | applemusic | Apple Music | music | 2 | first-party domains of the service |
| service | appletv | Apple TV+ | video | 1 | first-party domains of the service |
| service | badoo | Badoo | dating | 3 | first-party domains of the service |
| service | battlenet | Battle.net | gaming | 6 | first-party domains of the service |
| service | bbc | BBC | news | 4 | first-party domains of the service |
| service | bereal | BeReal | social | 3 | first-party domains of the service |
| service | bet365 | bet365 | gambling | 11 | first-party domains of the service |
| service | betfair | Betfair | gambling | 6 | first-party domains of the service |
| service | betway | Betway | gambling | 13 | first-party domains of the service |
| service | bilibili | Bilibili | video | 8 | first-party domains of the service |
| service | binance | Binance | crypto | 3 | first-party domains of the service |
| service | bluesky | Bluesky | social | 5 | first-party domains of the service; relay host from https://bsky.network/ |
| service | box | Box | productivity | 5 | first-party domains of the service; core domains from https://support.box.com/hc/en-us/articles/360043696434-Allowing-Box-Domains-on-Your-Network |
| service | bumble | Bumble | dating | 2 | first-party domains of the service |
| service | bybit | Bybit | crypto | 8 | first-party domains of the service |
| service | characterai | Character.AI | ai | 3 | first-party domains of the service |
| service | chatgpt | ChatGPT | ai | 6 | first-party domains of the service |
| service | claude | Claude | ai | 8 | first-party domains of the service; host names from https://claude.com/docs/claude-science/network-requirements |
| service | cnn | CNN | news | 3 | first-party domains of the service |
| service | coinbase | Coinbase | crypto | 2 | first-party domains of the service |
| service | copilot | Microsoft Copilot | ai | 2 | first-party domains of the service |
| service | crunchyroll | Crunchyroll | video | 3 | first-party domains of the service |
| service | cryptocom | Crypto.com | crypto | 1 | first-party domains of the service |
| service | cyberghost | CyberGhost | vpn | 2 | first-party domains of the service |
| service | dailymotion | Dailymotion | video | 3 | first-party domains of the service |
| service | deepseek | DeepSeek | ai | 1 | first-party domains of the service |
| service | deezer | Deezer | music | 2 | first-party domains of the service |
| service | discord | Discord | messaging | 11 | first-party domains of the service |
| service | disneyplus | Disney+ | video | 5 | first-party domains of the service |
| service | draftkings | DraftKings | gambling | 2 | first-party domains of the service |
| service | dropbox | Dropbox | productivity | 9 | first-party domains of the service; verified and user-content domains from https://help.dropbox.com/security/official-domains |
| service | ea | EA app | gaming | 4 | first-party domains of the service |
| service | ebay | eBay | shopping | 20 | first-party domains of the service |
| service | elevenlabs | ElevenLabs | ai | 2 | first-party domains of the service |
| service | epicgames | Epic Games | gaming | 2 | first-party domains of the service |
| service | etsy | Etsy | shopping | 3 | first-party domains of the service |
| service | expressvpn | ExpressVPN | vpn | 2 | first-party domains of the service |
| service | facebook | Facebook | social | 9 | first-party domains of the service |
| service | fanduel | FanDuel | gambling | 3 | first-party domains of the service |
| service | fortnite | Fortnite | gaming | 2 | first-party domains of the service |
| service | foxnews | Fox News | news | 3 | first-party domains of the service |
| service | gemini | Google Gemini | ai | 6 | first-party domains of the service; host names from https://knowledge.workspace.google.com/admin/gemini/gemini-app-firewall-settings and https://knowledge.workspace.google.com/admin/getting-started/set-up-a-google-workspace-host-name-allowlist |
| service | gog | GOG | gaming | 3 | first-party domains of the service |
| service | googledrive | Google Drive | productivity | 6 | first-party domains of the service; Drive and Docs host names from https://knowledge.workspace.google.com/admin/drive/drive-and-sites-firewall-and-proxy-settings |
| service | grindr | Grindr | dating | 3 | first-party domains of the service |
| service | grok | Grok | ai | 3 | first-party domains of the service |
| service | hinge | Hinge | dating | 2 | first-party domains of the service |
| service | hotspotshield | Hotspot Shield | vpn | 1 | first-party domains of the service |
| service | hoyoverse | HoYoverse (Genshin Impact, Honkai: Star Rail, Zenless Zone Zero) | gaming | 8 | first-party domains of the service |
| service | huggingface | Hugging Face | ai | 4 | first-party domains of the service |
| service | hulu | Hulu | video | 3 | first-party domains of the service |
| service | icloud | iCloud | productivity | 4 | first-party domains of the service; iCloud host names from https://support.apple.com/en-us/101555 |
| service | imgur | Imgur | social | 2 | first-party domains of the service |
| service | instagram | Instagram | social | 4 | first-party domains of the service |
| service | itchio | itch.io | gaming | 2 | first-party domains of the service |
| service | kakaotalk | KakaoTalk | messaging | 8 | first-party domains of the service; talk-apps host name from http://developers.kakao.com/docs/ko/reference/firewall |
| service | kick | Kick | video | 1 | first-party domains of the service |
| service | kraken | Kraken | crypto | 2 | first-party domains of the service |
| service | kucoin | KuCoin | crypto | 9 | first-party domains of the service |
| service | lemon8 | Lemon8 | social | 2 | first-party domains of the service |
| service | line | LINE | messaging | 6 | first-party domains of the service |
| service | linkedin | LinkedIn | social | 3 | first-party domains of the service |
| service | mastodon | Mastodon (mastodon.social) | social | 3 | first-party domains of the service |
| service | match | Match | dating | 1 | first-party domains of the service |
| service | max | HBO Max | video | 6 | first-party domains of the service |
| service | mediafire | MediaFire | productivity | 1 | first-party domains of the service |
| service | mega | MEGA | productivity | 4 | first-party domains of the service |
| service | metaai | Meta AI | ai | 1 | first-party domains of the service |
| service | midjourney | Midjourney | ai | 1 | first-party domains of the service |
| service | minecraft | Minecraft | gaming | 4 | first-party domains of the service |
| service | mistral | Mistral Vibe (Le Chat) | ai | 2 | first-party domains of the service |
| service | netflix | Netflix | video | 7 | first-party domains of the service |
| service | nintendo | Nintendo Switch Online | gaming | 9 | first-party domains of the service |
| service | nordvpn | NordVPN | vpn | 2 | first-party domains of the service |
| service | notion | Notion | productivity | 5 | first-party domains of the service; Notion-owned domains from https://www.notion.com/help/allowlist-ip |
| service | nytimes | The New York Times | news | 3 | first-party domains of the service |
| service | okcupid | OkCupid | dating | 2 | first-party domains of the service |
| service | okru | OK (Odnoklassniki) | social | 3 | first-party domains of the service |
| service | okx | OKX | crypto | 5 | first-party domains of the service |
| service | onedrive | OneDrive | productivity | 8 | first-party domains of the service; OneDrive host names from https://learn.microsoft.com/en-us/sharepoint/required-urls-and-ports |
| service | pandora | Pandora | music | 3 | first-party domains of the service |
| service | paramountplus | Paramount+ | video | 3 | first-party domains of the service |
| service | peacock | Peacock | video | 2 | first-party domains of the service |
| service | perplexity | Perplexity | ai | 3 | first-party domains of the service |
| service | pi | Pi | ai | 1 | first-party domains of the service |
| service | pinterest | Pinterest | social | 8 | first-party domains of the service |
| service | playstation | PlayStation Network | gaming | 3 | first-party domains of the service |
| service | plex | Plex | video | 4 | first-party domains of the service; plex.direct and app.plex.tv from https://support.plex.tv/articles/206225077-how-to-use-secure-server-connections/ |
| service | plutotv | Pluto TV | video | 2 | first-party domains of the service |
| service | poe | Poe | ai | 2 | first-party domains of the service |
| service | pof | Plenty of Fish | dating | 2 | first-party domains of the service |
| service | pokerstars | PokerStars | gambling | 12 | first-party domains of the service |
| service | primevideo | Amazon Prime Video | video | 5 | first-party domains of the service |
| service | privateinternetaccess | Private Internet Access | vpn | 2 | first-party domains of the service |
| service | protonvpn | Proton VPN | vpn | 1 | first-party domains of the service |
| service | psiphon | Psiphon | vpn | 2 | first-party domains of the service |
| service | pubg | PUBG: Battlegrounds | gaming | 4 | first-party domains of the service |
| service | quora | Quora | forums | 3 | first-party domains of the service |
| service | reddit | Reddit | social | 4 | first-party domains of the service |
| service | replicate | Replicate | ai | 4 | first-party domains of the service |
| service | riotgames | Riot Games (League of Legends, Valorant) | gaming | 5 | first-party domains of the service |
| service | roblox | Roblox | gaming | 5 | first-party domains of the service |
| service | rockstar | Rockstar Games | gaming | 3 | first-party domains of the service |
| service | runway | Runway | ai | 3 | first-party domains of the service |
| service | shein | SHEIN | shopping | 10 | first-party domains of the service |
| service | signal | Signal | messaging | 6 | first-party domains of the service |
| service | slack | Slack | productivity | 7 | first-party domains of the service; host names from https://slack.com/help/articles/360001603387-Manage-Slack-connection-issues |
| service | snapchat | Snapchat | social | 6 | first-party domains of the service |
| service | soundcloud | SoundCloud | music | 3 | first-party domains of the service |
| service | spotify | Spotify | music | 6 | first-party domains of the service |
| service | stability | Stability AI | ai | 2 | first-party domains of the service |
| service | stackexchange | Stack Overflow & Stack Exchange | forums | 8 | first-party domains of the service |
| service | stake | Stake | gambling | 6 | first-party domains of the service; mirror domains from https://help.stake.com/en/articles/16032252-official-stake-com-mirror-sites-what-they-are-how-to-use-them |
| service | steam | Steam | gaming | 9 | first-party domains of the service |
| service | suno | Suno | ai | 2 | first-party domains of the service |
| service | supercell | Supercell (Clash of Clans, Clash Royale, Brawl Stars) | gaming | 8 | first-party domains of the service |
| service | surfshark | Surfshark | vpn | 1 | first-party domains of the service |
| service | telegram | Telegram | messaging | 8 | first-party domains of the service |
| service | temu | Temu | shopping | 3 | first-party domains of the service |
| service | theguardian | The Guardian | news | 4 | first-party domains of the service |
| service | threads | Threads | social | 2 | first-party domains of the service |
| service | tidal | TIDAL | music | 2 | first-party domains of the service |
| service | tiktok | TikTok | social | 10 | first-party domains of the service |
| service | tinder | Tinder | dating | 2 | first-party domains of the service |
| service | torproject | Tor Project | vpn | 1 | first-party domains of the service |
| service | tubi | Tubi | video | 4 | first-party domains of the service |
| service | tumblr | Tumblr | social | 2 | first-party domains of the service |
| service | tunnelbear | TunnelBear | vpn | 1 | first-party domains of the service |
| service | twitch | Twitch | video | 6 | first-party domains of the service |
| service | twitter | X (Twitter) | social | 4 | first-party domains of the service |
| service | ubisoft | Ubisoft Connect | gaming | 4 | first-party domains of the service |
| service | valorant | VALORANT | gaming | 2 | first-party domains of the service |
| service | viber | Viber | messaging | 3 | first-party domains of the service |
| service | vimeo | Vimeo | video | 2 | first-party domains of the service |
| service | vk | VK | social | 7 | first-party domains of the service |
| service | walmart | Walmart | shopping | 4 | first-party domains of the service |
| service | wechat | WeChat | messaging | 6 | first-party domains of the service |
| service | weibo | Weibo | social | 4 | first-party domains of the service |
| service | wetransfer | WeTransfer | productivity | 3 | first-party domains of the service; host names from https://wetransfer.com/help-center/troubleshooting/configure-firewall |
| service | whatsapp | WhatsApp | messaging | 3 | first-party domains of the service |
| service | williamhill | William Hill | gambling | 4 | first-party domains of the service |
| service | windscribe | Windscribe | vpn | 2 | first-party domains of the service |
| service | xbox | Xbox network | gaming | 4 | first-party domains of the service; xboxlive.com and xbox.com from https://learn.microsoft.com/en-us/gaming/gdk/_content/gc/getstarted/config-dev-kit-software/configure-dev-network |
| service | youtube | YouTube | video | 10 | first-party domains of the service; YouTube host names from https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users |
| service | youtubemusic | YouTube Music | music | 1 | first-party domains of the service |
| service | zoom | Zoom | productivity | 4 | first-party domains of the service; core domains from https://support.zoom.com/hc/en/article?id=zm_kb&sysparm_article=KB0060548 |

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
