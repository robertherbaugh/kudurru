# Provenance: catalogue icons

SPDX-License-Identifier: Apache-2.0

| Field | Value |
|---|---|
| Files | `*.svg` (this directory), embedded into the kudurru binary by `internal/listdata/catalogue` (`go:embed`) and served as `GET /api/v1/catalogue/icons/{slug}.svg` |
| Purpose | logos of the blocked services and safe-search engines of the catalogue (`icon` in `catalogue.json`) and of well-known upstream DNS providers, shown by the dashboard |
| Upstream repository | https://github.com/homarr-labs/dashboard-icons |
| Upstream commit | `104a791794ef68b31bfecac676ddace8c223fd27` (main, 2026-10-11T09:30:25Z) |
| Fetched | 2026-10-11, `git clone --depth 1 https://github.com/homarr-labs/dashboard-icons.git`; the files are `svg/<slug>.svg` of that commit |
| Licence | Apache-2.0, Copyright (c) 2024 Bjorn Lammers, Meier Lukas, Thomas Camlong and Homarr Labs (`LICENSE` in this directory, copied from the same commit; SHA-256 `e58a8fa57781d1dc7158b333b68c99461031fd2c25dfbd23eac15a8e984cae56`) |
| Trademarks | the logos are trademarks of their owners. As the upstream README puts it: "All product names, trademarks, and registered trademarks are the property of their respective owners. Icons are used for identification purposes only and do not imply endorsement." The Apache-2.0 licence grants no trademark rights (section 6); kudurru shows a logo only to identify the service, search engine or DNS provider it names. |
| Modifications | the files marked `svgo` were run through svgo 4.0.0 (`multipass`, `preset-default`, `removeScripts`, `removeXlink`, `removeDimensions`, `removeAttrs` of `on*`); the others are byte-identical to upstream. No upstream file contained a script, an event handler or an external reference; `TestIconsSafe` keeps it so. |
| Size | 40 files, 75,900 bytes |
| Not embedded | `minecraft.svg` (88,959 bytes of pixel rectangles, over the size this directory aims for); services and engines dashboard-icons has no logo for (9gag, vk, vimeo, fortnite, roblox, tinder, pixabay) and Quad9 and OpenDNS (only the parent company's `cisco` logo exists). The dashboard draws a monogram for them. |

## Files

| Slug | Upstream file | Bytes | Changes | SHA-256 | Used for |
|---|---|---|---|---|---|
| `adguard-home` | `svg/adguard-home.svg` | 672 | unmodified | `4a65b23ae16bf700f35139e2c10a5f731a32530f2db832abd5612be2523b1f9f` | upstream provider AdGuard DNS |
| `bing` | `svg/bing.svg` | 5317 | svgo | `94afd95c01214f3f4f8ef46bf1e2e89fa6093ad1c82806f978a4e1206f4e8e66` | safe-search engine bing |
| `brave` | `svg/brave.svg` | 3086 | svgo | `21903b1a967c3d7379626dca8df0843e34dbc3b8ff243e021f1b771f3c800357` | safe-search engine brave |
| `chatgpt` | `svg/chatgpt.svg` | 871 | svgo | `f8380191fd4c30007556e5634161d5978a4e00bd5e05beff899502417cdf88b4` | service chatgpt |
| `cisco` | `svg/cisco.svg` | 1755 | svgo | `94abf1ba7472d34f9a1c62d3a8c1796c10378a6ac19adaaf26d7c6d164db6817` | upstream provider OpenDNS (Cisco) |
| `cloudflare` | `svg/cloudflare.svg` | 1216 | unmodified | `8d1880046ed223ffb4ca660d4f52cff986d6db310e25d87cd82eccd024b461f5` | upstream provider Cloudflare |
| `control-d-dark` | `svg/control-d-dark.svg` | 643 | svgo | `cb3e26bb8b4b2cb98a559900f621f7df7da0fc949ee584169114fe1b6a44b7bb` | `control-d` on dark backgrounds |
| `control-d` | `svg/control-d.svg` | 643 | svgo | `3cf475343f693ef87a630bfbac964d90d538ff9388370f54b0c43fdc313752d1` | upstream provider Control D |
| `discord` | `svg/discord.svg` | 779 | unmodified | `c04059c1d13d99239ebd0b7df807e8c3d2f46439bd18a70c5fd3a0631bd7b310` | service discord |
| `disney-plus` | `svg/disney-plus.svg` | 5710 | unmodified | `cdc480ea45b4fe48c3503e118d763f959ec9affdb645c1a71467b24b108a15ee` | service disneyplus |
| `duckduckgo` | `svg/duckduckgo.svg` | 5108 | svgo | `85365c95f2d3762774bb64ecf820a59285e57d4252f5803e649c004e0282bc95` | safe-search engine duckduckgo |
| `epic-games` | `svg/epic-games.svg` | 7055 | svgo | `8f92a7a51832c4c9536985b9c24cff495051b3aead1dda9d98e0e9b2a9fd4429` | service epicgames |
| `facebook` | `svg/facebook.svg` | 758 | unmodified | `d2963ecd06de4e61d9cdae95b464d5055a289122c9726793f5a7485386f7fe66` | service facebook |
| `google` | `svg/google.svg` | 817 | unmodified | `b68e01a0dd2b0cc6a3bcb9d61419f39a7722dd88517bfbf2904214a6d84aa2f3` | safe-search engine google; upstream provider Google Public DNS |
| `hulu` | `svg/hulu.svg` | 819 | unmodified | `b1edb8d6ffa25fa030d9dbba46f0f686e6873d3d35b5f4cddc73dc913823b7e1` | service hulu |
| `instagram` | `svg/instagram.svg` | 3285 | unmodified | `2c1ad008f5d7f2abd8f7aa1b7f1c7c87859efb0204aee8f5bc88d12567179550` | service instagram |
| `linkedin` | `svg/linkedin.svg` | 627 | unmodified | `67f08e714e9a8a06288f1d7adc7e9840c450c11f8af6fb1847425fc7f186ca1b` | service linkedin |
| `mullvad` | `svg/mullvad.svg` | 2040 | unmodified | `dff18d10f95bbae43d251bf84e467e6d045b3c7fb66fdd9151fe72801ab76f62` | upstream provider Mullvad DNS |
| `netflix` | `svg/netflix.svg` | 3338 | svgo | `85acd5201c6f409da67b9dfa3d861ca56883f63b031e1f04683d73da4fd5a06d` | service netflix |
| `nextdns` | `svg/nextdns.svg` | 923 | unmodified | `3526f19120fa80d0f43cee90f7b85ae3abcd5eb83889270ba17c0009fc119353` | upstream provider NextDNS |
| `pinterest` | `svg/pinterest.svg` | 992 | unmodified | `e3f1783f07a0add791adcbc86eabd009717691e0c541b97c23364e2b11b19dd8` | service pinterest |
| `prime-video` | `svg/prime-video.svg` | 6821 | svgo | `ee33b66b0e97d0baf5b774b6fa71c465896eb8fab76fb7b8d2c2d03a5a9ad54f` | service primevideo |
| `reddit` | `svg/reddit.svg` | 973 | unmodified | `6af5a5941885fbe62ea15eca389b608ce06ea89d42ec43d1de827cfe82ef6880` | service reddit |
| `riot` | `svg/riot.svg` | 2036 | svgo | `daf3e105dbeb5971a1b9fa49394290639fb28c721bb0a2da713a41d68278f754` | service riotgames |
| `signal` | `svg/signal.svg` | 1671 | unmodified | `437ffbaa9a9f7ac579e40d267e0a55f83e66eefb716b3b4a05a2ab70ce7fd4e7` | service signal |
| `snapchat` | `svg/snapchat.svg` | 4625 | unmodified | `0e5fabffd2827f6b2684c743559b93cd454dae51594054fb3b066997a4038763` | service snapchat |
| `spotify` | `svg/spotify.svg` | 1193 | svgo | `e4e5b9605c888c7762a5d94b71f8737f04fcb49ce2aa160445027b68afbea148` | service spotify |
| `steam` | `svg/steam.svg` | 1488 | svgo | `308dea604c3bc03233a93da848d01cd7bf43ad29ab429ed59cd719c7337eea9c` | service steam |
| `telegram` | `svg/telegram.svg` | 1007 | unmodified | `29edf9cfe2d4c80eb0acd087312eaabba8f7b4c14a4651ccaab51c35b3bb608c` | service telegram |
| `threads-light` | `svg/threads-light.svg` | 1158 | unmodified | `972d6a443b27333621cb8a12adae6f3cda81a3c22aeebe52422302e0af254065` | `threads` on dark backgrounds |
| `threads` | `svg/threads.svg` | 1143 | unmodified | `619c277e7e5487bb45c22e6d4563fed6478db45ffe35b524d01d740e45de1f3f` | service threads |
| `tiktok-light` | `svg/tiktok-light.svg` | 1378 | unmodified | `29c63bafd4358b4140e6c0e74b2ad0e0a131155d0d23ef946d95ba050fba49ef` | `tiktok` on dark backgrounds |
| `tiktok` | `svg/tiktok.svg` | 1366 | unmodified | `919df6a0deca75be9fd4220458cde7debfbb8207b73ba5b48c09a887e6863d7f` | service tiktok |
| `tumblr` | `svg/tumblr.svg` | 759 | unmodified | `d7fa114f6d8e9b5cb617e088b2b73e075146d1059dc713f680b5aff8ec44c622` | service tumblr |
| `twitch` | `svg/twitch.svg` | 585 | svgo | `d3a93cf72b2628127680debdf4f839149d6053af91929c85086a02aa2cac5905` | service twitch |
| `whatsapp` | `svg/whatsapp.svg` | 2061 | unmodified | `629ff81469c1200a00c3ce70d829ae8faa4872fa4f786dad890d64966695fd42` | service whatsapp |
| `x-light` | `svg/x-light.svg` | 246 | unmodified | `380c48a830f28c36efd4bfc1af7e2c01a85d12b9d948c667e31cb414bdbb33e4` | `x` on dark backgrounds |
| `x` | `svg/x.svg` | 231 | unmodified | `eef86bcbbd5cf2b889be343261c64561c7514e19b7c0dc32c937f0c43d477998` | service twitter (X) |
| `yandex` | `svg/yandex.svg` | 226 | svgo | `228cf4e435a1f698be3c087d50baf6d46098b45e35ece572201f3de7817c2e69` | safe-search engine yandex |
| `youtube` | `svg/youtube.svg` | 479 | unmodified | `320ae220c53a6126da81aaf6803d8c813837bcaa95f39dcf5f75932335746673` | service and safe-search engine youtube |

## Updating

Clone dashboard-icons at a newer commit, copy the `svg/<slug>.svg` files
wanted into an empty directory, run svgo with the options above, copy the
results here and rewrite the table (slug, bytes, SHA-256) and the commit
above. `TestIconProvenance` fails when a file is missing from the table or
its SHA-256 differs, and `TestIconsSafe` when a file carries a script, an
event handler, a `foreignObject`, an entity declaration or a reference
outside the document. Name a new icon in `catalogue.json` with `icon` when
its slug differs from the entry's ID.
