# Provenance: blocked-services and safe-search catalogue

SPDX-License-Identifier: Apache-2.0

| Field | Value |
|---|---|
| File | `catalogue.json` (this directory), embedded into the kudurru binary by `internal/listdata/catalogue` (`go:embed`) |
| Purpose | `Policy.spec.blockedServices` and `Policy.spec.safeSearch` (docs/architecture.md §7, D60) |
| Schema version | 2 |
| Catalogue version | `2026.10.12` |
| SHA-256 | `110d1368d6f2d6c38d23c0555f54b9ea195d3f74c95abb79edfa9caefe0a300f` |
| Size | 73,250 bytes, 15 categories, 150 services, 7 safe-search engines (74 services name a logo, `icon`: see `icons/PROVENANCE.md`) |
| License | Apache-2.0, Copyright 2026 Robert Herbaugh and the kudurru contributors (`LICENSE` in this directory). The rest of the repository is under the PolyForm Noncommercial License 1.0.0; this data stays Apache-2.0 on purpose (D164), so it can be published and reused on its own |
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
| service | 4chan | 4chan | forums | 3 | first-party domains of the service; host names from https://github.com/4chan/4chan-API and https://www.4chan.org/faq |
| service | 9gag | 9GAG | social | 2 | first-party domains of the service |
| service | aliexpress | AliExpress | shopping | 4 | first-party domains of the service; host names from https://www.aliexpress.com/ and https://aliexpress.ru/ |
| service | amazon | Amazon | shopping | 23 | first-party domains of the service; country sites from https://www.amazon.com/customer-preferences/country |
| service | applemusic | Apple Music | music | 1 | first-party domains of the service; host names from https://support.apple.com/guide/music-web/welcome/web |
| service | appletv | Apple TV+ | video | 1 | first-party domains of the service; host names from https://www.apple.com/apple-tv-plus/ |
| service | badoo | Badoo | dating | 2 | first-party domains of the service; host names from https://hackerone.com/bumble/policy_scopes |
| service | battlenet | Battle.net | gaming | 5 | first-party domains of the service; host names from https://overwatch.blizzard.com/en-us/ and https://www.blizzard.cn/ |
| service | bbc | BBC | news | 3 | first-party domains of the service; bbci.co.uk from https://www.bbc.co.uk/ |
| service | bereal | BeReal | social | 3 | first-party domains of the service |
| service | bet365 | bet365 | gambling | 11 | first-party domains of the service; licensed domains from https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/55149 and the other pages under Domain citations |
| service | betfair | Betfair | gambling | 8 | first-party domains of the service; country domains from https://betfair-developer-docs.atlassian.net/wiki/spaces/1smk3cen4v3lu3yomq5qye0ni/pages/2687915/Login+Session+Management and the other pages under Domain citations |
| service | betway | Betway | gambling | 11 | first-party domains of the service; licensed and country domains from https://mgaurlchecker.mga.org.mt/ and the other pages under Domain citations |
| service | bilibili | Bilibili | video | 8 | first-party domains of the service |
| service | binance | Binance | crypto | 3 | first-party domains of the service; host names from https://www.binance.com/en/official-verification and https://x.com/BinanceHelpDesk/status/1464954119832154114 |
| service | bluesky | Bluesky | social | 5 | first-party domains of the service; relay host from https://bsky.network/ |
| service | box | Box | productivity | 5 | first-party domains of the service; core domains from https://docs.box.com/en/box-admin-tools/box-security/configuring-a-firewall-for-box-applications-and-services |
| service | bumble | Bumble | dating | 1 | first-party domains of the service |
| service | bybit | Bybit | crypto | 9 | first-party domains of the service; official domains from https://www.bybit.com/en/verification/ |
| service | characterai | Character.AI | ai | 2 | first-party domains of the service; host names from https://support.character.ai/hc/en-us/articles/51616603958427-Creator-Leaderboard |
| service | chatgpt | ChatGPT | ai | 6 | first-party domains of the service; host names from https://help.openai.com/en/articles/9247338-network-recommendations-for-chatgpt-errors-on-web-and-apps and https://openai.com/index/sora-is-here/ |
| service | claude | Claude | ai | 5 | first-party domains of the service; host names from https://claude.com/docs/claude-science/network-requirements and https://claude.com/docs/third-party/claude-desktop/telemetry |
| service | cnn | CNN | news | 3 | first-party domains of the service; host names from https://x.com/CNN and https://www.cnn.com/ |
| service | coinbase | Coinbase | crypto | 1 | first-party domains of the service |
| service | copilot | Microsoft Copilot | ai | 2 | first-party domains of the service; copilot.com (its canonical address) from https://copilot.microsoft.com/ |
| service | crunchyroll | Crunchyroll | video | 2 | first-party domains of the service |
| service | cryptocom | Crypto.com | crypto | 1 | first-party domains of the service |
| service | cyberghost | CyberGhost | vpn | 1 | first-party domains of the service |
| service | dailymotion | Dailymotion | video | 3 | first-party domains of the service |
| service | deepseek | DeepSeek | ai | 1 | first-party domains of the service; host names from https://api-docs.deepseek.com/ |
| service | deezer | Deezer | music | 2 | first-party domains of the service |
| service | discord | Discord | messaging | 11 | first-party domains of the service |
| service | disneyplus | Disney+ | video | 5 | first-party domains of the service; bamgrid.com and disneystreaming.com from https://specs.disneystreaming.com/media-metadata-feed/spec.html, dssott.com from ESPN's own tag configuration https://dcf.espn.com/TWDC-DTCI/prod/Bootstrap.js |
| service | draftkings | DraftKings | gambling | 1 | first-party domains of the service; licensed host names from https://igamingontario.ca/en/player/regulated-igaming-market |
| service | dropbox | Dropbox | productivity | 9 | first-party domains of the service; verified and user-content domains from https://help.dropbox.com/security/official-domains |
| service | ea | EA app | gaming | 1 | first-party domains of the service |
| service | ebay | eBay | shopping | 21 | first-party domains of the service; country sites from https://developer.ebay.com/api-docs/static/rest-request-components.html, https://developer.ebay.com/devzone/xml/docs/reference/ebay/types/PictureDetailsType.html and https://www.ebay.com/ |
| service | elevenlabs | ElevenLabs | ai | 1 | first-party domains of the service |
| service | epicgames | Epic Games | gaming | 1 | first-party domains of the service; host names from https://www.epicgames.com/help/c-202300000001639/a202300000012358 |
| service | etsy | Etsy | shopping | 2 | first-party domains of the service; host names from https://www.etsy.com/ |
| service | expressvpn | ExpressVPN | vpn | 1 | first-party domains of the service |
| service | facebook | Facebook | social | 8 | first-party domains of the service; host names from http://web.archive.org/web/20250616231514/https://www.workplace.com/resources/tech/it-configuration/networking and the other pages under Domain citations |
| service | fanduel | FanDuel | gambling | 2 | first-party domains of the service; licensed domains from https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/43990 and https://igamingontario.ca/en/player/regulated-igaming-market |
| service | fortnite | Fortnite | gaming | 1 | first-party domains of the service |
| service | foxnews | Fox News | news | 1 | first-party domains of the service |
| service | gemini | Google Gemini | ai | 6 | first-party domains of the service; host names from https://knowledge.workspace.google.com/admin/gemini/gemini-app-firewall-settings and the other pages under Domain citations |
| service | gog | GOG | gaming | 3 | first-party domains of the service; host names from https://www.gog.com/en/ and https://www.gog.com/galaxy |
| service | googledrive | Google Drive | productivity | 6 | first-party domains of the service; Drive and Docs host names from https://knowledge.workspace.google.com/admin/drive/drive-and-sites-firewall-and-proxy-settings |
| service | grindr | Grindr | dating | 2 | first-party domains of the service; host names from https://hackerone.com/grindr/policy_scopes |
| service | grok | Grok | ai | 3 | first-party domains of the service; host names from https://docs.x.ai/build/enterprise.md and https://grok.com/ |
| service | hinge | Hinge | dating | 1 | first-party domains of the service |
| service | hotspotshield | Hotspot Shield | vpn | 1 | first-party domains of the service |
| service | hoyoverse | HoYoverse (Genshin Impact, Honkai: Star Rail, Zenless Zone Zero) | gaming | 5 | first-party domains of the service |
| service | huggingface | Hugging Face | ai | 3 | first-party domains of the service; host names from https://huggingface.co/docs/hub/models-downloading and https://huggingface.co/docs/hub/spaces-embed |
| service | hulu | Hulu | video | 3 | first-party domains of the service |
| service | icloud | iCloud | productivity | 4 | first-party domains of the service; iCloud host names from https://support.apple.com/en-us/101555 |
| service | imgur | Imgur | social | 2 | first-party domains of the service |
| service | instagram | Instagram | social | 3 | first-party domains of the service; host names from https://developers.facebook.com/documentation/business-messaging/instagram-messaging/features/ig-me-links and https://developers.facebook.com/documentation/threads/threads-profiles |
| service | itchio | itch.io | gaming | 2 | first-party domains of the service; host names from https://itch.io/ |
| service | kakaotalk | KakaoTalk | messaging | 2 | first-party domains of the service; host names from https://developers.kakao.com/docs/ko/reference/firewall and https://open.kakao.com/ |
| service | kick | Kick | video | 1 | first-party domains of the service |
| service | kraken | Kraken | crypto | 2 | first-party domains of the service; host names from https://www.kraken.com/ |
| service | kucoin | KuCoin | crypto | 9 | first-party domains of the service; official domains from https://www.kucoin.com/cert |
| service | lemon8 | Lemon8 | social | 2 | first-party domains of the service |
| service | line | LINE | messaging | 5 | first-party domains of the service; line-apps.com from https://github.com/line/line-openapi/blob/main/messaging-api.yml, line-scdn.net from https://developers.line.biz/en/docs/liff/developing-liff-apps/ |
| service | linkedin | LinkedIn | social | 3 | first-party domains of the service |
| service | mastodon | Mastodon (mastodon.social) | social | 3 | first-party domains of the service; host names from https://joinmastodon.org/about |
| service | match | Match | dating | 1 | first-party domains of the service |
| service | max | HBO Max | video | 3 | first-party domains of the service; HBO Max and the max.com rename from https://press.wbd.com/us/media-release/warner-bros-discovery-announces-max-become-hbo-max-summer |
| service | mediafire | MediaFire | productivity | 1 | first-party domains of the service |
| service | mega | MEGA | productivity | 3 | first-party domains of the service; official domains from https://help.mega.io/security/data-protection/official-domains |
| service | metaai | Meta AI | ai | 1 | first-party domains of the service; host names from https://about.fb.com/news/2024/04/meta-ai-assistant-built-with-llama-3/ |
| service | midjourney | Midjourney | ai | 1 | first-party domains of the service |
| service | minecraft | Minecraft | gaming | 3 | first-party domains of the service; host names from https://edusupport.minecraft.net/hc/en-us/articles/360047118992 |
| service | mistral | Mistral Vibe (Le Chat) | ai | 1 | first-party domains of the service |
| service | netflix | Netflix | video | 8 | Netflix's own list of the domains its apps use, https://openconnect.netflix.com/mobiledeliverydomains.txt; netflix.net from https://openconnect.zendesk.com/hc/en-us/articles/360000911251 |
| service | nintendo | Nintendo Switch Online | gaming | 2 | first-party domains of the service; nintendo.net from https://www.nintendo.com/us/ and https://ec.nintendo.com/ |
| service | nordvpn | NordVPN | vpn | 1 | first-party domains of the service |
| service | notion | Notion | productivity | 5 | first-party domains of the service; Notion-owned domains from https://www.notion.com/help/allowlist-ip and https://www.notion.com/help/network-control |
| service | nytimes | The New York Times | news | 3 | first-party domains of the service; host names from https://github.com/nytimes/public_api_specs/blob/master/top_stories/top_stories_v2.md and https://x.com/NYTScience |
| service | okcupid | OkCupid | dating | 2 | first-party domains of the service; host names from https://www.okcupid.com/press |
| service | okru | OK (Odnoklassniki) | social | 3 | first-party domains of the service |
| service | okx | OKX | crypto | 4 | first-party domains of the service; official domains from https://www.okx.com/support-center/channel-verification |
| service | onedrive | OneDrive | productivity | 8 | first-party domains of the service; OneDrive host names from https://learn.microsoft.com/en-us/sharepoint/required-urls-and-ports and the other pages under Domain citations |
| service | pandora | Pandora | music | 3 | first-party domains of the service |
| service | paramountplus | Paramount+ | video | 3 | first-party domains of the service |
| service | peacock | Peacock | video | 2 | first-party domains of the service |
| service | perplexity | Perplexity | ai | 3 | first-party domains of the service; host names from https://status.perplexity.com/ and https://x.com/perplexity_ai/status/1717953875678794158 |
| service | pi | Pi | ai | 1 | first-party domains of the service; host names from https://inflection.ai/ |
| service | pinterest | Pinterest | social | 8 | first-party domains of the service |
| service | playstation | PlayStation Network | gaming | 3 | first-party domains of the service; host names from https://www.playstation.com/en-us/ |
| service | plex | Plex | video | 2 | first-party domains of the service; plex.direct and app.plex.tv from https://support.plex.tv/articles/206225077-how-to-use-secure-server-connections/ |
| service | plutotv | Pluto TV | video | 2 | first-party domains of the service |
| service | poe | Poe | ai | 2 | first-party domains of the service; host names from https://creator.poe.com/docs/script-bots/quick-start |
| service | pof | Plenty of Fish | dating | 2 | first-party domains of the service |
| service | pokerstars | PokerStars | gambling | 11 | first-party domains of the service; licensed domains from https://mgaurlchecker.mga.org.mt/ and the other pages under Domain citations |
| service | primevideo | Amazon Prime Video | video | 5 | first-party domains of the service |
| service | privateinternetaccess | Private Internet Access | vpn | 3 | first-party domains of the service; host names from https://github.com/pia-foss/manual-connections and https://github.com/pia-foss/desktop |
| service | protonvpn | Proton VPN | vpn | 1 | first-party domains of the service |
| service | psiphon | Psiphon | vpn | 2 | first-party domains of the service; host names from https://psiphon.ca/en/download.html |
| service | pubg | PUBG: Battlegrounds | gaming | 3 | first-party domains of the service |
| service | quora | Quora | forums | 2 | first-party domains of the service; host names from https://help.quora.com/hc/en-us/articles/360022808612 |
| service | reddit | Reddit | social | 4 | first-party domains of the service |
| service | replicate | Replicate | ai | 4 | first-party domains of the service; host names from https://replicate.com/docs/topics/predictions/output-files, https://replicate.com/ and https://github.com/replicate/cog |
| service | riotgames | Riot Games (League of Legends, Valorant) | gaming | 4 | first-party domains of the service; host names from https://playvalorant.com/en-us/download/ |
| service | roblox | Roblox | gaming | 3 | first-party domains of the service; host names from https://en.help.roblox.com/hc/en-us/articles/115005744663 and https://hackerone.com/roblox/policy_scopes |
| service | rockstar | Rockstar Games | gaming | 1 | first-party domains of the service |
| service | runway | Runway | ai | 2 | first-party domains of the service; host names from https://help.runwayml.com/hc/en-us/articles/51474557342227-Identifying-and-troubleshooting-network-issues |
| service | shein | SHEIN | shopping | 9 | first-party domains of the service; country sites (the page's alternate-language links) from https://www.shein.co.uk/ |
| service | signal | Signal | messaging | 6 | first-party domains of the service |
| service | slack | Slack | productivity | 7 | first-party domains of the service; host names from https://docs.slack.dev/faq and the other pages under Domain citations |
| service | snapchat | Snapchat | social | 5 | first-party domains of the service; host names from https://developers.snap.com/marketing-api/Ads-API/snap-pixel.md and https://developers.snap.com/snap-kit/login-kit/Tutorials/web.md |
| service | soundcloud | SoundCloud | music | 3 | first-party domains of the service |
| service | spotify | Spotify | music | 6 | first-party domains of the service |
| service | stability | Stability AI | ai | 2 | first-party domains of the service; host names from https://stability.ai/news-updates/sharing-a-new-way-to-work-with-stable-audio |
| service | stackexchange | Stack Overflow & Stack Exchange | forums | 8 | first-party domains of the service; host names from https://api.stackexchange.com/docs/types/site and https://stackexchange.com/sites?view=list |
| service | stake | Stake | gambling | 8 | first-party domains of the service; mirror and country domains from https://help.stake.com/en/articles/16032252-official-stake-com-mirror-sites-what-they-are-how-to-use-them and the other pages under Domain citations |
| service | steam | Steam | gaming | 7 | first-party domains of the service; host names from https://help.steampowered.com/en/faqs/view/2EA8-4D75-DA21-31EB and https://api.steampowered.com/ISteamDirectory/GetCMListForConnect/v1/?cellid=0 |
| service | suno | Suno | ai | 2 | first-party domains of the service; host names from https://github.com/suno-ai/bark |
| service | supercell | Supercell (Clash of Clans, Clash Royale, Brawl Stars) | gaming | 6 | first-party domains of the service |
| service | surfshark | Surfshark | vpn | 1 | first-party domains of the service |
| service | telegram | Telegram | messaging | 7 | first-party domains of the service; link hosts from https://github.com/tdlib/td/blob/master/td/telegram/LinkManager.cpp (see PROVENANCE.md) |
| service | temu | Temu | shopping | 3 | first-party domains of the service; host names from https://www.temu.com/ |
| service | theguardian | The Guardian | news | 3 | first-party domains of the service; host names from https://open-platform.theguardian.com/documentation/search and https://www.theguardian.com/ |
| service | threads | Threads | social | 2 | first-party domains of the service; host names from https://developers.facebook.com/documentation/threads/overview |
| service | tidal | TIDAL | music | 2 | first-party domains of the service |
| service | tiktok | TikTok | social | 10 | first-party domains of the service |
| service | tinder | Tinder | dating | 2 | first-party domains of the service; host names from https://hackerone.com/tinder/policy_scopes |
| service | torproject | Tor Project | vpn | 1 | first-party domains of the service |
| service | tubi | Tubi | video | 5 | first-party domains of the service; tubi.com, tubi.tv and tubitv.com from https://tubitv.com/static/terms |
| service | tumblr | Tumblr | social | 2 | first-party domains of the service |
| service | tunnelbear | TunnelBear | vpn | 1 | first-party domains of the service |
| service | twitch | Twitch | video | 6 | first-party domains of the service |
| service | twitter | X (Twitter) | social | 4 | first-party domains of the service; host names from https://help.x.com/en/using-x/url-shortener and https://docs.x.com/x-api/fundamentals/data-dictionary |
| service | ubisoft | Ubisoft Connect | gaming | 2 | first-party domains of the service; host names from https://www.ubisoft.com/en-us/ubisoft-connect/download |
| service | valorant | VALORANT | gaming | 1 | first-party domains of the service |
| service | viber | Viber | messaging | 3 | first-party domains of the service |
| service | vimeo | Vimeo | video | 2 | first-party domains of the service |
| service | vk | VK | social | 7 | first-party domains of the service |
| service | walmart | Walmart | shopping | 4 | first-party domains of the service; host names from https://corporate.walmart.com/about/international/markets/canada and https://www.walmart.com/ |
| service | wechat | WeChat | messaging | 6 | first-party domains of the service |
| service | weibo | Weibo | social | 4 | first-party domains of the service |
| service | wetransfer | WeTransfer | productivity | 3 | first-party domains of the service; host names from https://wetransfer.com/help-center/troubleshooting/configure-firewall |
| service | whatsapp | WhatsApp | messaging | 3 | first-party domains of the service; host names from http://web.archive.org/web/20241228071158/https://developers.facebook.com/docs/whatsapp/guides/network-requirements/ and https://faq.whatsapp.com/5913398998672934 |
| service | williamhill | William Hill | gambling | 3 | first-party domains of the service; licensed domains from https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/39225 and the other pages under Domain citations |
| service | windscribe | Windscribe | vpn | 2 | first-party domains of the service; host names from https://github.com/Windscribe/wsnet |
| service | xbox | Xbox network | gaming | 4 | first-party domains of the service; host names from https://learn.microsoft.com/en-us/gaming/gdk/_content/gc/getstarted/config-dev-kit-software/configure-dev-network, https://www.xbox.com/en-US/ and https://www.xbox.com/en-US/play |
| service | youtube | YouTube | video | 7 | first-party domains of the service; YouTube host names from https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users and https://support.google.com/youtubekids/answer/9406390 |
| service | youtubemusic | YouTube Music | music | 1 | first-party domains of the service |
| service | zoom | Zoom | productivity | 4 | first-party domains of the service; core domains from https://support.zoom.com/hc/en/article?id=zm_kb&sysparm_article=KB0060548 and https://www.zoomgov.com/ |

## Domain citations

The vendor page each host name of these entries comes from, where an
entry draws on more than one page. A domain that no vendor page names
and that is not the service's own brand domain is left out.

| Service | Domains | Page |
|---|---|---|
| 4chan | `4cdn.org` | https://github.com/4chan/4chan-API |
| 4chan | `4channel.org` | https://www.4chan.org/faq |
| aliexpress | `aliexpress.us`, `aliexpress-media.com` | https://www.aliexpress.com/ |
| aliexpress | `aliexpress.ru` | https://aliexpress.ru/ |
| amazon | `amazon.com`, `amazon.co.uk`, `amazon.de`, `amazon.ie`, `amazon.co.za` | https://www.amazon.com/customer-preferences/country |
| applemusic | `music.apple.com` | https://support.apple.com/guide/music-web/welcome/web |
| appletv | `tv.apple.com` | https://www.apple.com/apple-tv-plus/ |
| badoo | `badoo.com`, `badoocdn.com` | https://hackerone.com/bumble/policy_scopes |
| battlenet | `playoverwatch.com` | https://overwatch.blizzard.com/en-us/ |
| battlenet | `blizzard.cn`, `battlenet.com.cn` | https://www.blizzard.cn/ |
| bbc | `bbci.co.uk` | https://www.bbc.co.uk/ |
| bet365 | `bet365.com` | https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/55149 |
| bet365 | `bet365.es` | https://www.ordenacionjuego.es/operadores-juego/operadores-licencia/operadores |
| bet365 | `bet365.it` | https://www.adm.gov.it/portale/monopoli/giochi/gioco_distanza/gioco_dist_concessionari |
| bet365 | `bet365.de` | https://www.gluecksspiel-behoerde.de/de/fuer-spielende/uebersicht-erlaubter-anbieter-whitelist |
| bet365 | `bet365.dk` | https://spillemyndigheden.dk/spiludbydere-med-tilladelse |
| bet365 | `bet365.gr` | https://certifications.gamingcommission.gov.gr/publicRecordsOnline/SitePages/KatoxoiAdeiasOnline.aspx |
| bet365 | `bet365.nl` | https://openovergokken.nl/kansspelwijzer/hillside-new-media-malta-plc/ |
| bet365 | `bet365.ca` | https://igamingontario.ca/en/player/regulated-igaming-market |
| bet365 | `bet365.com.au` | https://www.acma.gov.au/check-if-gambling-operator-legal |
| bet365 | `bet365.bet.br` | https://www.gov.br/fazenda/pt-br/composicao/orgaos/secretaria-de-premios-e-apostas/transparencia-ativa-processos-de-autorizacao-de-apostas-de-quota-fixa/empresas-autorizadas |
| bet365 | `bet365.fr` | https://anj.fr/offre-de-jeu-et-marche/operateurs-agrees |
| betfair | `betfair.it`, `betfair.es`, `betfair.ro`, `betfair.com.au`, `betfair.bet.br`, `betfair.se` | https://betfair-developer-docs.atlassian.net/wiki/spaces/1smk3cen4v3lu3yomq5qye0ni/pages/2687915/Login+Session+Management |
| betfair | `betfair.com` | https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/39435 |
| betfair | `betfair.dk` | https://spillemyndigheden.dk/spiludbydere-med-tilladelse |
| betway | `betway.com`, `betway.ie` | https://mgaurlchecker.mga.org.mt/ |
| betway | `betway.es` | https://www.ordenacionjuego.es/operadores-juego/operadores-licencia/operadores |
| betway | `betway.de` | https://www.gluecksspiel-behoerde.de/de/fuer-spielende/uebersicht-erlaubter-anbieter-whitelist |
| betway | `betway.ca` | https://igamingontario.ca/en/player/regulated-igaming-market |
| betway | `betway.be` | https://betway.be/maint-betway?maintenancePage=betway-be-market-close |
| betway | `betway.mx` | https://betway.mx/ |
| betway | `betway.co.za` | https://www.betway.co.za/ |
| betway | `betway.com.gh` | https://www.betway.com.gh/ |
| betway | `betway.com.ng` | https://www.betway.com.ng/ |
| betway | `betway.co.zm` | https://www.betway.co.zm/ |
| binance | `binance.com`, `binance.us` | https://www.binance.com/en/official-verification |
| binance | `bin.bnbstatic.com` | https://x.com/BinanceHelpDesk/status/1464954119832154114 |
| box | `box.com`, `box.net`, `boxcdn.net`, `boxcloud.com`, `app.boxenterprise.net` | https://docs.box.com/en/box-admin-tools/box-security/configuring-a-firewall-for-box-applications-and-services |
| bybit | `bybit.com`, `bybit.eu`, `bybitglobal.com`, `bybit-global.com`, `bybit.kz`, `bybit.tr`, `bybit-tr.com`, `bybitgeorgia.ge`, `bybit.id` | https://www.bybit.com/en/verification/ |
| characterai | `c.ai` | https://support.character.ai/hc/en-us/articles/51616603958427-Creator-Leaderboard |
| chatgpt | `chatgpt.com`, `openai.com`, `oaistatic.com`, `oaiusercontent.com`, `oaistatsig.com` | https://help.openai.com/en/articles/9247338-network-recommendations-for-chatgpt-errors-on-web-and-apps |
| chatgpt | `sora.com` | https://openai.com/index/sora-is-here/ |
| claude | `claude.ai`, `claude.com`, `api.anthropic.com`, `claudemcpcontent.com` | https://claude.com/docs/claude-science/network-requirements |
| claude | `claudeusercontent.com` | https://claude.com/docs/third-party/claude-desktop/telemetry |
| cnn | `cnn.it` | https://x.com/CNN |
| cnn | `cnn.io` | https://www.cnn.com/ |
| copilot | `copilot.com` | https://copilot.microsoft.com/ |
| deepseek | `api.deepseek.com` | https://api-docs.deepseek.com/ |
| draftkings | `draftkings.com` | https://igamingontario.ca/en/player/regulated-igaming-market |
| ebay | `ebay.com`, `ebay.co.uk`, `ebay.de`, `ebay.com.hk`, `ebay.com.my`, `ebay.ph`, `ebay.com.tw` | https://developer.ebay.com/api-docs/static/rest-request-components.html |
| ebay | `ebayimg.com` | https://developer.ebay.com/devzone/xml/docs/reference/ebay/types/PictureDetailsType.html |
| ebay | `ebaystatic.com` | https://www.ebay.com/ |
| epicgames | `epicgames.com` | https://www.epicgames.com/help/c-202300000001639/a202300000012358 |
| etsy | `etsystatic.com` | https://www.etsy.com/ |
| facebook | `facebook.com`, `fbcdn.net`, `fb.me`, `fbsbx.com` | http://web.archive.org/web/20250616231514/https://www.workplace.com/resources/tech/it-configuration/networking |
| facebook | `facebook.net` | https://developers.facebook.com/documentation/javascript/quickstart |
| facebook | `m.me` | https://developers.facebook.com/documentation/business-messaging/messenger-platform/discovery/m-me-links |
| fanduel | `fanduel.com` | https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/43990 |
| fanduel | `fanduel.ca` | https://igamingontario.ca/en/player/regulated-igaming-market |
| gemini | `gemini.google.com`, `gemini.google` | https://knowledge.workspace.google.com/admin/gemini/gemini-app-firewall-settings |
| gemini | `aistudio.google.com` | https://knowledge.workspace.google.com/admin/getting-started/set-up-a-google-workspace-host-name-allowlist |
| gemini | `generativelanguage.googleapis.com` | https://ai.google.dev/api |
| gemini | `bard.google.com` | https://blog.google/technology/ai/try-bard/ |
| gemini | `makersuite.google.com` | https://developers.googleblog.com/en/make-with-makersuite-part-1-an-introduction/ |
| gog | `gog-statics.com` | https://www.gog.com/en/ |
| gog | `gogalaxy.com` | https://www.gog.com/galaxy |
| grindr | `grindr.com`, `grindr.mobi` | https://hackerone.com/grindr/policy_scopes |
| grok | `grok.com`, `x.ai` | https://docs.x.ai/build/enterprise.md |
| grok | `grokusercontent.com` | https://grok.com/ |
| huggingface | `hf.co` | https://huggingface.co/docs/hub/models-downloading |
| huggingface | `hf.space` | https://huggingface.co/docs/hub/spaces-embed |
| instagram | `ig.me` | https://developers.facebook.com/documentation/business-messaging/instagram-messaging/features/ig-me-links |
| instagram | `cdninstagram.com` | https://developers.facebook.com/documentation/threads/threads-profiles |
| itchio | `itch.zone` | https://itch.io/ |
| kakaotalk | `talk-apps.kakao.com` | https://developers.kakao.com/docs/ko/reference/firewall |
| kakaotalk | `open.kakao.com` | https://open.kakao.com/ |
| kraken | `krak.app` | https://www.kraken.com/ |
| kucoin | `kucoin.com`, `kucoinauth.cloud`, `kucoin.club`, `kucoin.cloud`, `kucoin.biz`, `kucoin.plus`, `kucoin.work`, `kucoin.tr`, `kucoin.th` | https://www.kucoin.com/cert |
| mastodon | `mastodon.social`, `mastodon.online` | https://joinmastodon.org/about |
| mega | `mega.io`, `mega.nz`, `mega.co.nz` | https://help.mega.io/security/data-protection/official-domains |
| metaai | `meta.ai` | https://about.fb.com/news/2024/04/meta-ai-assistant-built-with-llama-3/ |
| minecraft | `minecraft-services.net` | https://edusupport.minecraft.net/hc/en-us/articles/360047118992 |
| nintendo | `cdn.nintendo.net` | https://www.nintendo.com/us/ |
| nintendo | `srv.nintendo.net` | https://ec.nintendo.com/ |
| notion | `notion.com`, `notion.site`, `notionusercontent.com`, `notion-static.com` | https://www.notion.com/help/allowlist-ip |
| notion | `notion.so` | https://www.notion.com/help/network-control |
| nytimes | `nyt.com` | https://github.com/nytimes/public_api_specs/blob/master/top_stories/top_stories_v2.md |
| nytimes | `nyti.ms` | https://x.com/NYTScience |
| okcupid | `okccdn.com` | https://www.okcupid.com/press |
| okx | `okx.com`, `okx.ac`, `okx.cab`, `okx.reise` | https://www.okx.com/support-center/channel-verification |
| onedrive | `onedrive.live.com`, `onedrive.com`, `1drv.com`, `storage.live.com`, `skyapi.live.net`, `skydrive.live.com`, `livefilestore.com` | https://learn.microsoft.com/en-us/sharepoint/required-urls-and-ports |
| onedrive | `1drv.ms` | https://support.microsoft.com/en-us/office/share-files-and-folders-in-microsoft-onedrive-9fcc2f7d-de0c-4cec-93b0-a82024800c07 |
| perplexity | `perplexity.com` | https://status.perplexity.com/ |
| perplexity | `pplx.ai` | https://x.com/perplexity_ai/status/1717953875678794158 |
| pi | `pi.ai` | https://inflection.ai/ |
| playstation | `playstation.net`, `sonyentertainmentnetwork.com` | https://www.playstation.com/en-us/ |
| poe | `poecdn.net` | https://creator.poe.com/docs/script-bots/quick-start |
| pokerstars | `pokerstars.com`, `pokerstars.eu`, `pokerstarscasino.com` | https://mgaurlchecker.mga.org.mt/ |
| pokerstars | `pokerstars.uk` | https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/39108 |
| pokerstars | `pokerstars.it` | https://www.adm.gov.it/portale/monopoli/giochi/gioco_distanza/gioco_dist_concessionari |
| pokerstars | `pokerstars.es` | https://www.ordenacionjuego.es/operadores-juego/operadores-licencia/operadores |
| pokerstars | `pokerstars.fr` | https://anj.fr/offre-de-jeu-et-marche/operateurs-agrees |
| pokerstars | `pokerstars.de` | https://www.gluecksspiel-behoerde.de/de/fuer-spielende/uebersicht-erlaubter-anbieter-whitelist |
| pokerstars | `pokerstars.se` | https://www.spelinspektionen.se/licens-o-tillstand/licensregister/ |
| pokerstars | `pokerstars.dk` | https://spillemyndigheden.dk/spiludbydere-med-tilladelse |
| pokerstars | `pokerstars.gr` | https://certifications.gamingcommission.gov.gr/publicRecordsOnline/SitePages/KatoxoiAdeiasOnline.aspx |
| privateinternetaccess | `serverlist.piaservers.net` | https://github.com/pia-foss/manual-connections |
| privateinternetaccess | `api.piaproxy.net` | https://github.com/pia-foss/desktop |
| psiphon | `psiphon3.com` | https://psiphon.ca/en/download.html |
| quora | `qr.ae` | https://help.quora.com/hc/en-us/articles/360022808612 |
| replicate | `replicate.delivery` | https://replicate.com/docs/topics/predictions/output-files |
| replicate | `replicateassets.com` | https://replicate.com/ |
| replicate | `replicate.ai` | https://github.com/replicate/cog |
| riotgames | `riotcdn.net` | https://playvalorant.com/en-us/download/ |
| roblox | `roblox.com`, `rbxcdn.com` | https://en.help.roblox.com/hc/en-us/articles/115005744663 |
| roblox | `rbx.com` | https://hackerone.com/roblox/policy_scopes |
| runway | `runwayml.com`, `runwayml.cloud` | https://help.runwayml.com/hc/en-us/articles/51474557342227-Identifying-and-troubleshooting-network-issues |
| shein | `shein.co.uk`, `shein.com.mx`, `shein.com.vn`, `ltwebstatic.com` | https://www.shein.co.uk/ |
| slack | `slack.com`, `slack-edge.com`, `slackb.com` | https://docs.slack.dev/faq |
| slack | `slack-msgs.com` | https://docs.slack.dev/changelog/2017/04/01/start-using-rtm-connect-and-stop-using-rtm-start |
| slack | `slack-files.com` | https://docs.slack.dev/messaging/working-with-files |
| slack | `slack-redir.net` | https://slack.com/help/articles/204399413 |
| slack | `slack-imgs.com` | https://api.slack.com/robots |
| stability | `stableaudio.com` | https://stability.ai/news-updates/sharing-a-new-way-to-work-with-stable-audio |
| stackexchange | `sstatic.net` | https://api.stackexchange.com/docs/types/site |
| stackexchange | `stackoverflow.com`, `superuser.com`, `serverfault.com`, `askubuntu.com`, `mathoverflow.net`, `stackapps.com` | https://stackexchange.com/sites?view=list |
| stake | `playstake.club`, `playstake.info`, `playstake.io`, `playstake.casino` | https://help.stake.com/en/articles/16032252-official-stake-com-mirror-sites-what-they-are-how-to-use-them |
| stake | `stake.us` | https://help.stake.us/en/ |
| stake | `stake.bet.br` | https://www.gov.br/fazenda/pt-br/composicao/orgaos/secretaria-de-premios-e-apostas/transparencia-ativa-processos-de-autorizacao-de-apostas-de-quota-fixa/empresas-autorizadas |
| stake | `stake.it` | https://www.adm.gov.it/portale/monopoli/giochi/gioco_distanza/gioco_dist_concessionari |
| steam | `steampowered.com`, `steamcommunity.com`, `steamgames.com`, `steamusercontent.com`, `steamcontent.com`, `steamstatic.com` | https://help.steampowered.com/en/faqs/view/2EA8-4D75-DA21-31EB |
| steam | `steamserver.net` | https://api.steampowered.com/ISteamDirectory/GetCMListForConnect/v1/?cellid=0 |
| suno | `suno.ai` | https://github.com/suno-ai/bark |
| temu | `kwcdn.com`, `temucdn.com` | https://www.temu.com/ |
| theguardian | `guardianapis.com` | https://open-platform.theguardian.com/documentation/search |
| theguardian | `guim.co.uk` | https://www.theguardian.com/ |
| threads | `threads.net`, `threads.com` | https://developers.facebook.com/documentation/threads/overview |
| tinder | `gotinder.com` | https://hackerone.com/tinder/policy_scopes |
| twitter | `t.co` | https://help.x.com/en/using-x/url-shortener |
| twitter | `twimg.com` | https://docs.x.com/x-api/fundamentals/data-dictionary |
| ubisoft | `ubi.com` | https://www.ubisoft.com/en-us/ubisoft-connect/download |
| walmart | `walmart.ca` | https://corporate.walmart.com/about/international/markets/canada |
| walmart | `walmartimages.com`, `wal.co` | https://www.walmart.com/ |
| whatsapp | `whatsapp.net`, `whatsapp.com` | http://web.archive.org/web/20241228071158/https://developers.facebook.com/docs/whatsapp/guides/network-requirements/ |
| whatsapp | `wa.me` | https://faq.whatsapp.com/5913398998672934 |
| williamhill | `williamhill.com` | https://www.gamblingcommission.gov.uk/public-register/business/detail/domain-names/39225 |
| williamhill | `williamhill.es` | https://www.ordenacionjuego.es/operadores-juego/operadores-licencia/operadores |
| williamhill | `williamhill.it` | https://www.adm.gov.it/portale/monopoli/giochi/gioco_distanza/gioco_dist_concessionari |
| windscribe | `windscribe.net` | https://github.com/Windscribe/wsnet |
| xbox | `xboxlive.com`, `xbox.com` | https://learn.microsoft.com/en-us/gaming/gdk/_content/gc/getstarted/config-dev-kit-software/configure-dev-network |
| xbox | `xboxservices.com` | https://www.xbox.com/en-US/ |
| xbox | `gamepass.com` | https://www.xbox.com/en-US/play |
| youtube | `youtube.com`, `youtu.be`, `ytimg.com`, `youtube-nocookie.com`, `youtubei.googleapis.com`, `youtube.googleapis.com` | https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users |
| youtube | `youtubekids.com` | https://support.google.com/youtubekids/answer/9406390 |
| zoom | `zoom.us`, `zoom.com`, `zoomapp.cloud` | https://support.zoom.com/hc/en/article?id=zm_kb&sysparm_article=KB0060548 |
| zoom | `zoomgov.com` | https://www.zoomgov.com/ |

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

## Re-check of 2026-10-12 (messaging, social, video)

Domains taken from DNS or redirects only were re-checked against first-party
sources. Removed for lack of one, or as no longer used: `snapads.com`
(Snapchat; no address, parked), `cdn-telegram.org` (Telegram; media moved to
`telesco.pe`), `line-cdn.net` (LINE), `hbo.com` (HBO's brand and linear-TV
site, not the streaming app), `hbogo.com` and `hbonow.com` (closed
services), `vrv.co` (folded into Crunchyroll, redirect only). Added:
`nflxsearch.net` (Netflix's own domain list) and `tubi.com` (Tubi's terms).
Telegram's link hosts `t.me`, `telegram.me` and `telegram.dog` are named in
TDLib (`td/telegram/LinkManager.cpp`), `telesco.pe` in
https://telegram.org/blog/video-messages-and-telescope, `telegra.ph` in
https://telegram.org/blog/telegraph and `tg.dev` in
https://github.com/TelegramMessenger/telegram-login-ios. Snapchat's
`sc-cdn.net` is named in www.snapchat.com's own response headers. Disney's
`bamgrid.com` and `dssott.com` are shared with Hulu and ESPN, and HBO Max's
`discomax.com` with discovery+; the entries' notes say so. Domains still
listed without a first-party page carry their vendor's nameservers and
redirects as evidence: `pin.it`, Pinterest's country domains,
`odnoklassniki.ru`, `weibocdn.com`, `bereal.network`, `biliapi.net`,
`biliapi.com`, `pscdn.co`, `p-cdn.us`, `tidalhifi.com`, `huluim.com`,
`hulustream.com` and the Prime Video CDN domains.
