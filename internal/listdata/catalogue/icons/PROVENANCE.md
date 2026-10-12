# Provenance: catalogue icons

SPDX-License-Identifier: Apache-2.0

| Field | Value |
|---|---|
| Files | `*.svg` (this directory), embedded into the kudurru binary by `internal/listdata/catalogue` (`go:embed`) and served as `GET /api/v1/catalogue/icons/{slug}.svg` |
| Purpose | logos of the blocked services and safe-search engines of the catalogue (`icon` in `catalogue.json`) and of well-known upstream DNS providers, shown by the dashboard |
| Upstream repository | https://github.com/homarr-labs/dashboard-icons |
| Upstream commit | `104a791794ef68b31bfecac676ddace8c223fd27` (main, 2026-10-11T09:30:25Z) |
| Fetched | 2026-10-11, `git clone --depth 1 https://github.com/homarr-labs/dashboard-icons.git`; the files added on 2026-10-12 were downloaded from `https://raw.githubusercontent.com/homarr-labs/dashboard-icons/<commit>/svg/<slug>.svg` at the same commit. The files are `svg/<slug>.svg` of that commit |
| Licence | Apache-2.0, Copyright (c) 2024 Bjorn Lammers, Meier Lukas, Thomas Camlong and Homarr Labs (`LICENSE` in this directory, copied from the same commit; SHA-256 `e58a8fa57781d1dc7158b333b68c99461031fd2c25dfbd23eac15a8e984cae56`) |
| Trademarks | the logos are trademarks of their owners. As the upstream README puts it: "All product names, trademarks, and registered trademarks are the property of their respective owners. Icons are used for identification purposes only and do not imply endorsement." The Apache-2.0 licence grants no trademark rights (section 6); kudurru shows a logo only to identify the service, search engine or DNS provider it names. |
| Modifications | the files marked `svgo` were run through svgo 4.0.0 (`multipass`, `preset-default`, `removeScripts`, `removeXlink`, `removeDimensions`, `removeAttrs` of `on*`); the others are byte-identical to upstream. No upstream file contained a script, an event handler or an external reference; `google-gemini` uses SVG filter primitives (feGaussianBlur, feFlood, feBlend), which compute from the document only. `TestIconsSafe` keeps it so. |
| Size | 97 files, 170,597 bytes |
| Not embedded | `minecraft.svg` (88,959 bytes of pixel rectangles) and `hugging-face.svg` (18,681 bytes), over the 16 KiB per file this directory allows; `wetransfer.svg` (an embedded raster image); `eleven-labs.svg` (a white wordmark, invisible on light backgrounds); `claude-ai-light.svg` and `claude-ai-dark.svg` (PNG files under an .svg name upstream); services and engines dashboard-icons has no logo for (9gag, vk, vimeo, fortnite, roblox, tinder, pixabay, most dating, gambling, news, crypto and smaller gaming and VPN brands) and Quad9 and OpenDNS (only the parent company's `cisco` logo exists). The dashboard draws a monogram for them. `meta` (Meta's company logo) stands for Meta AI. |

## Files

| Slug | Upstream file | Bytes | Changes | SHA-256 | Used for |
|---|---|---|---|---|---|
| `4chan` | `svg/4chan.svg` | 11860 | svgo | `b919d59f691ae22648f6a4765d44f1ac546f3b4107bd6e10af14b8ece76fc9df` | service 4chan |
| `adguard-home` | `svg/adguard-home.svg` | 672 | unmodified | `4a65b23ae16bf700f35139e2c10a5f731a32530f2db832abd5612be2523b1f9f` | upstream provider AdGuard DNS |
| `aliexpress` | `svg/aliexpress.svg` | 828 | svgo | `27c6abc93785a19c0648b22a09218d3e1149563b4138e1d884cfab9c1463c565` | service aliexpress |
| `amazon` | `svg/amazon.svg` | 2221 | svgo | `2636276071a6d0916a52e45ba8e66c0543db956010fb27f00e5d03166cb2a8e3` | service amazon |
| `amazon-light` | `svg/amazon-light.svg` | 2269 | svgo | `7826055997df8031db05731235067238a9076a2e5eba8ab5efe53be4624490e8` | `amazon` on dark backgrounds |
| `apple-music` | `svg/apple-music.svg` | 2122 | unmodified | `e2ef7e7f0b608eedd0af93bce3ad0cf208628ec67650d9b16052c5bdc47dad69` | service applemusic |
| `apple-tv-plus` | `svg/apple-tv-plus.svg` | 823 | unmodified | `dcfd56cfd5502524617588fbee79b05f3eeb5180135de5307bf035515d42b28c` | service appletv |
| `apple-tv-plus-light` | `svg/apple-tv-plus-light.svg` | 835 | unmodified | `cca92ccfc0d8b34a0383988919377c4a1aca57af87d845757fe1dba4a6ec8c54` | `apple-tv-plus` on dark backgrounds |
| `bilibili` | `svg/bilibili.svg` | 2956 | svgo | `e375d93b5189471102b1019f9033743df4bcc87396cafbd3232d69f8dbafa681` | service bilibili |
| `bing` | `svg/bing.svg` | 5317 | svgo | `94afd95c01214f3f4f8ef46bf1e2e89fa6093ad1c82806f978a4e1206f4e8e66` | safe-search engine bing |
| `bluesky` | `svg/bluesky.svg` | 525 | unmodified | `0cfadae29283a01d2023f1a13fd9da77935921c6185d4eef0f52aa2d0147a14d` | service bluesky |
| `box` | `svg/box.svg` | 1087 | unmodified | `ff5d8ff316b1e2cc32c5a511d965bbdea75598a71474235538bed6744241e07d` | service box |
| `brave` | `svg/brave.svg` | 3086 | svgo | `21903b1a967c3d7379626dca8df0843e34dbc3b8ff243e021f1b771f3c800357` | safe-search engine brave |
| `chatgpt` | `svg/chatgpt.svg` | 871 | svgo | `f8380191fd4c30007556e5634161d5978a4e00bd5e05beff899502417cdf88b4` | service chatgpt |
| `cisco` | `svg/cisco.svg` | 1755 | svgo | `94abf1ba7472d34f9a1c62d3a8c1796c10378a6ac19adaaf26d7c6d164db6817` | upstream provider OpenDNS (Cisco) |
| `claude-ai` | `svg/claude-ai.svg` | 2153 | svgo | `eb8ce5a69e36ed034c9141095b8ab7e5e6853be3625983724c9ae9a8fb65207d` | service claude |
| `cloudflare` | `svg/cloudflare.svg` | 1216 | unmodified | `8d1880046ed223ffb4ca660d4f52cff986d6db310e25d87cd82eccd024b461f5` | upstream provider Cloudflare |
| `control-d` | `svg/control-d.svg` | 643 | svgo | `3cf475343f693ef87a630bfbac964d90d538ff9388370f54b0c43fdc313752d1` | upstream provider Control D |
| `control-d-dark` | `svg/control-d-dark.svg` | 643 | svgo | `cb3e26bb8b4b2cb98a559900f621f7df7da0fc949ee584169114fe1b6a44b7bb` | `control-d` on dark backgrounds |
| `crunchyroll` | `svg/crunchyroll.svg` | 1594 | svgo | `d7606d6d581972e6a7e4385aa6671b769f26374bbdc46a612d713e1af93023cf` | service crunchyroll |
| `deepseek` | `svg/deepseek.svg` | 2365 | svgo | `109fb1a61f4e57651cb0200c5b8ddaa22808921a193b3069b92b421b2840730b` | service deepseek |
| `deezer` | `svg/deezer.svg` | 1200 | svgo | `b75eeea00d2367b39378deb44482fdff9f6e0a17202b8c1defe6d20f150a66ce` | service deezer |
| `discord` | `svg/discord.svg` | 779 | unmodified | `c04059c1d13d99239ebd0b7df807e8c3d2f46439bd18a70c5fd3a0631bd7b310` | service discord |
| `disney-plus` | `svg/disney-plus.svg` | 5710 | unmodified | `cdc480ea45b4fe48c3503e118d763f959ec9affdb645c1a71467b24b108a15ee` | service disneyplus |
| `dropbox` | `svg/dropbox.svg` | 417 | unmodified | `16fd819b4f0deac21770d225828f8e0789492f9209a0984a850169776778a43f` | service dropbox |
| `duckduckgo` | `svg/duckduckgo.svg` | 5108 | svgo | `85365c95f2d3762774bb64ecf820a59285e57d4252f5803e649c004e0282bc95` | safe-search engine duckduckgo |
| `ebay` | `svg/ebay.svg` | 1574 | svgo | `1e908c966f78db561ffe664835a3792fd01bdde0ac7a7191d990fbed2034a923` | service ebay |
| `electronic-arts` | `svg/electronic-arts.svg` | 527 | unmodified | `7c5152ec421a7c3cfc4dd8809cb555fe94bf5f56a2b27a3a3a4ab6f796c89a78` | service ea |
| `epic-games` | `svg/epic-games.svg` | 7055 | svgo | `8f92a7a51832c4c9536985b9c24cff495051b3aead1dda9d98e0e9b2a9fd4429` | service epicgames |
| `expressvpn` | `svg/expressvpn.svg` | 518 | svgo | `ed9bf83edcb1b9974b787fd188d6abaa3de0273082ef42234640c4182711ad46` | service expressvpn |
| `facebook` | `svg/facebook.svg` | 758 | unmodified | `d2963ecd06de4e61d9cdae95b464d5055a289122c9726793f5a7485386f7fe66` | service facebook |
| `google` | `svg/google.svg` | 817 | unmodified | `b68e01a0dd2b0cc6a3bcb9d61419f39a7722dd88517bfbf2904214a6d84aa2f3` | safe-search engine google; upstream provider Google Public DNS |
| `google-drive` | `svg/google-drive.svg` | 800 | unmodified | `963477d7e4a0b0d8865dd7aec8e27d8fd9c3a4b4f2e5b81f9df8581f9f2eca11` | service googledrive |
| `google-gemini` | `svg/google-gemini.svg` | 7661 | svgo | `7fdc195ecfff4eddcd10049401665e590a4e470d28f8ec47a39d63a1ad1a4bea` | service gemini |
| `grok` | `svg/grok.svg` | 685 | svgo | `d9347cdf64ffef9aaf19d1f7f1866b29d0c726bdf5c36e65f41e8aac96916145` | service grok |
| `grok-dark` | `svg/grok-dark.svg` | 685 | svgo | `24376f39f78a1f8f329046a33f9749ed9457c716615e5968df77c15e48834b50` | `grok` on dark backgrounds |
| `hbo` | `svg/hbo.svg` | 991 | svgo | `0f6db3cfa76723733dd75c2b32910e9a2a012ff900e9da2fcc9767a9e0aea31f` | service max |
| `hbo-light` | `svg/hbo-light.svg` | 1015 | svgo | `dacc693c7f0cc1c4c97017377e64feb5ae2e6a4c5c703aa55ff43613de651cdd` | `hbo` on dark backgrounds |
| `hulu` | `svg/hulu.svg` | 819 | unmodified | `b1edb8d6ffa25fa030d9dbba46f0f686e6873d3d35b5f4cddc73dc913823b7e1` | service hulu |
| `icloud` | `svg/icloud.svg` | 941 | svgo | `69b0bec74dd50a7d38cfae09e4fd6eb19cf7caebe195425bb5abdd19e939e769` | service icloud |
| `instagram` | `svg/instagram.svg` | 3285 | unmodified | `2c1ad008f5d7f2abd8f7aa1b7f1c7c87859efb0204aee8f5bc88d12567179550` | service instagram |
| `kick` | `svg/kick.svg` | 339 | unmodified | `4cb7b56d98e3a9466d8755ebd02b186544cfaed5bfbea0acf7e7d82764675764` | service kick |
| `linkedin` | `svg/linkedin.svg` | 627 | unmodified | `67f08e714e9a8a06288f1d7adc7e9840c450c11f8af6fb1847425fc7f186ca1b` | service linkedin |
| `mastodon` | `svg/mastodon.svg` | 1421 | unmodified | `c5d0bbb046587569e575ebda4ae9f32da308f6ecfebe0b84ed0c615b974bc969` | service mastodon |
| `mediafire` | `svg/mediafire.svg` | 848 | svgo | `c817e715ca2bd3459087b06e52a4538c80fdc7585498c8528671642585c83452` | service mediafire |
| `mega-nz` | `svg/mega-nz.svg` | 237 | svgo | `fb53a2cb7910ee386bc64e73b1e2fe71a4a2bf63a161a87762c792e567c7febe` | service mega |
| `meta` | `svg/meta.svg` | 1901 | svgo | `68219ee71b87ad98b3fedc5d63490738754982d3b58334e58e51e6bb34526d67` | service metaai |
| `microsoft-copilot` | `svg/microsoft-copilot.svg` | 3286 | svgo | `ce37af3526c6f863c249762f0d65700c11d3c2faf2ac522752ac085385b8e991` | service copilot |
| `microsoft-onedrive` | `svg/microsoft-onedrive.svg` | 5600 | svgo | `c1782ed73404c3a9a2a35e07496ebaa7db6e890a3cf66fb499098c904333b83c` | service onedrive |
| `mistral-ai` | `svg/mistral-ai.svg` | 2081 | svgo | `28da2b326214d98bd40d285aa91b9cc4e954dfc106d236d9b9564c4cdbf2c934` | service mistral |
| `mullvad` | `svg/mullvad.svg` | 2040 | unmodified | `dff18d10f95bbae43d251bf84e467e6d045b3c7fb66fdd9151fe72801ab76f62` | upstream provider Mullvad DNS |
| `netflix` | `svg/netflix.svg` | 3338 | svgo | `85acd5201c6f409da67b9dfa3d861ca56883f63b031e1f04683d73da4fd5a06d` | service netflix |
| `nextdns` | `svg/nextdns.svg` | 923 | unmodified | `3526f19120fa80d0f43cee90f7b85ae3abcd5eb83889270ba17c0009fc119353` | upstream provider NextDNS |
| `nintendo-switch` | `svg/nintendo-switch.svg` | 1672 | svgo | `487fc20fa91e7c893c587aca53f7658d5f0b5e18f79580ca398f9328e40ce662` | service nintendo |
| `nordvpn` | `svg/nordvpn.svg` | 298 | unmodified | `77548d4543437429217f34d9a7506e279cba920f376c5ee9ae84693c35c0aa24` | service nordvpn |
| `notion` | `svg/notion.svg` | 1141 | unmodified | `b98fea4bc3f3259c6907a40dae994c959c3240d7ee4b4afea144a555c638f6c2` | service notion |
| `paramount-plus` | `svg/paramount-plus.svg` | 2394 | svgo | `81b593a258773c994006dfbecff00493a61fd8d486653881afaba524ab170efe` | service paramountplus |
| `peacock` | `svg/peacock.svg` | 1139 | unmodified | `3e25492d3da3a2753c3eef6f0525d13a89de7e3ba42aecaf83c00f73c1919b89` | service peacock |
| `peacock-light` | `svg/peacock-light.svg` | 1151 | unmodified | `6d182d04bf2f327edaa7d38c157a4689558cd8e56d846800dad1c23d1b97fe9e` | `peacock` on dark backgrounds |
| `perplexity` | `svg/perplexity.svg` | 509 | svgo | `bd2e9d3102dfb96b27ffb90214d771ddc408a3f4844fa6b1ed29db1afb35e302` | service perplexity |
| `pinterest` | `svg/pinterest.svg` | 992 | unmodified | `e3f1783f07a0add791adcbc86eabd009717691e0c541b97c23364e2b11b19dd8` | service pinterest |
| `playstation` | `svg/playstation.svg` | 1240 | unmodified | `7e0e9f79bbc01cb128cbc05f90c6199472f29babf48193bc8c7585ff65dd68cf` | service playstation |
| `plex` | `svg/plex.svg` | 191 | unmodified | `ae6e754dc2644c7ad6b41853cdac75812ccaeed4c2f08fcd88d81e2f3ec01dba` | service plex |
| `plex-light` | `svg/plex-light.svg` | 188 | unmodified | `65108e09609b3788fa56e7d289652517b666029d64a9511ebd9285f9737ccc8d` | `plex` on dark backgrounds |
| `prime-video` | `svg/prime-video.svg` | 6821 | svgo | `ee33b66b0e97d0baf5b774b6fa71c465896eb8fab76fb7b8d2c2d03a5a9ad54f` | service primevideo |
| `private-internet-access` | `svg/private-internet-access.svg` | 3474 | unmodified | `4ec34356dd412ba5d81d90e301ccbf11fbd761d7d08c99be9f3344c6578cc880` | service privateinternetaccess |
| `proton-vpn` | `svg/proton-vpn.svg` | 1033 | unmodified | `70155118c7b1e0fbcd26723dd15e118d2e79bffd447a3fafa63631ccf7b3e997` | service protonvpn |
| `reddit` | `svg/reddit.svg` | 973 | unmodified | `6af5a5941885fbe62ea15eca389b608ce06ea89d42ec43d1de827cfe82ef6880` | service reddit |
| `riot` | `svg/riot.svg` | 2036 | svgo | `daf3e105dbeb5971a1b9fa49394290639fb28c721bb0a2da713a41d68278f754` | service riotgames |
| `signal` | `svg/signal.svg` | 1671 | unmodified | `437ffbaa9a9f7ac579e40d267e0a55f83e66eefb716b3b4a05a2ab70ce7fd4e7` | service signal |
| `slack` | `svg/slack.svg` | 968 | unmodified | `62e556a75b94516fd8dcfa9c8ee4eae76268b4087ee927d91a5f2d3115d54118` | service slack |
| `snapchat` | `svg/snapchat.svg` | 4625 | unmodified | `0e5fabffd2827f6b2684c743559b93cd454dae51594054fb3b066997a4038763` | service snapchat |
| `soundcloud` | `svg/soundcloud.svg` | 1300 | svgo | `dac7430c5bec9113da4160133a9c9570e033fc57a6d5b2729da81d28d5fdead6` | service soundcloud |
| `soundcloud-dark` | `svg/soundcloud-dark.svg` | 1292 | svgo | `2791ea30d95eac4e7b51fd97b3ab9479e51a4678bd0486dced4ad6a5d030de63` | `soundcloud` on dark backgrounds |
| `spotify` | `svg/spotify.svg` | 1193 | svgo | `e4e5b9605c888c7762a5d94b71f8737f04fcb49ce2aa160445027b68afbea148` | service spotify |
| `steam` | `svg/steam.svg` | 1488 | svgo | `308dea604c3bc03233a93da848d01cd7bf43ad29ab429ed59cd719c7337eea9c` | service steam |
| `surfshark` | `svg/surfshark.svg` | 1589 | svgo | `fb51103261d51322f7a9125e4c6f4c5bf95d62f75ef5abe53bd3ba0dcd356358` | service surfshark |
| `telegram` | `svg/telegram.svg` | 1007 | unmodified | `29edf9cfe2d4c80eb0acd087312eaabba8f7b4c14a4651ccaab51c35b3bb608c` | service telegram |
| `threads` | `svg/threads.svg` | 1143 | unmodified | `619c277e7e5487bb45c22e6d4563fed6478db45ffe35b524d01d740e45de1f3f` | service threads |
| `threads-light` | `svg/threads-light.svg` | 1158 | unmodified | `972d6a443b27333621cb8a12adae6f3cda81a3c22aeebe52422302e0af254065` | `threads` on dark backgrounds |
| `tidal` | `svg/tidal.svg` | 1396 | svgo | `ebed3b1e5a68f2a4c6aae00c9a5a121fabd035fd607b755c1a59dc6791c6d601` | service tidal |
| `tidal-dark` | `svg/tidal-dark.svg` | 1384 | svgo | `fafbcf38e1c58c15df389e4854415cecf0e02ac1fcc9718d3bd5e45ec6c9d9d4` | `tidal` on dark backgrounds |
| `tiktok` | `svg/tiktok.svg` | 1366 | unmodified | `919df6a0deca75be9fd4220458cde7debfbb8207b73ba5b48c09a887e6863d7f` | service tiktok |
| `tiktok-light` | `svg/tiktok-light.svg` | 1378 | unmodified | `29c63bafd4358b4140e6c0e74b2ad0e0a131155d0d23ef946d95ba050fba49ef` | `tiktok` on dark backgrounds |
| `tumblr` | `svg/tumblr.svg` | 759 | unmodified | `d7fa114f6d8e9b5cb617e088b2b73e075146d1059dc713f680b5aff8ec44c622` | service tumblr |
| `twitch` | `svg/twitch.svg` | 585 | svgo | `d3a93cf72b2628127680debdf4f839149d6053af91929c85086a02aa2cac5905` | service twitch |
| `viber` | `svg/viber.svg` | 2136 | svgo | `a0554883215e347a85602a34405ed4ead781b4c6f45ced9c8223323e08e5820a` | service viber |
| `wechat` | `svg/wechat.svg` | 1315 | svgo | `acc0695564784e115b0ef898ce1f1b67729a246671009260ecc6df5db28b105c` | service wechat |
| `weibo` | `svg/weibo.svg` | 2406 | svgo | `0b48d9a8106c73d685c19a168ce1b875b2f3d8bacc0f164cf6d44caa64fae73f` | service weibo |
| `whatsapp` | `svg/whatsapp.svg` | 2061 | unmodified | `629ff81469c1200a00c3ce70d829ae8faa4872fa4f786dad890d64966695fd42` | service whatsapp |
| `x` | `svg/x.svg` | 231 | unmodified | `eef86bcbbd5cf2b889be343261c64561c7514e19b7c0dc32c937f0c43d477998` | service twitter (X) |
| `x-light` | `svg/x-light.svg` | 246 | unmodified | `380c48a830f28c36efd4bfc1af7e2c01a85d12b9d948c667e31cb414bdbb33e4` | `x` on dark backgrounds |
| `xbox` | `svg/xbox.svg` | 1385 | unmodified | `f12d88f5a0fab740b6dd53081531435c66fe5bdace51a9f4a22ef1aa54e7a539` | service xbox |
| `yandex` | `svg/yandex.svg` | 226 | svgo | `228cf4e435a1f698be3c087d50baf6d46098b45e35ece572201f3de7817c2e69` | safe-search engine yandex |
| `youtube` | `svg/youtube.svg` | 479 | unmodified | `320ae220c53a6126da81aaf6803d8c813837bcaa95f39dcf5f75932335746673` | service and safe-search engine youtube |
| `youtube-music` | `svg/youtube-music.svg` | 338 | unmodified | `3d92bd8506a96b566f1ca995096421072eddef097ac9d50b50eacb72d33d95e4` | service youtubemusic |
| `zoom` | `svg/zoom.svg` | 393 | unmodified | `02a7a0b2028eb5ab7a9259ec0f9e59e8293244691b535aefdeb2b106023f760c` | service zoom |

## Updating

Clone dashboard-icons at a newer commit, copy the `svg/<slug>.svg` files
wanted into an empty directory, run svgo with the options above, copy the
results here and rewrite the table (slug, bytes, SHA-256) and the commit
above. `TestIconProvenance` fails when a file is missing from the table or
its SHA-256 differs, and `TestIconsSafe` when a file carries a script, an
event handler, a `foreignObject`, an entity declaration or a reference
outside the document. Name a new icon in `catalogue.json` with `icon` when
its slug differs from the entry's ID.
