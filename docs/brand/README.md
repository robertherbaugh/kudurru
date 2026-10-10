# kudurru branding brief

A minimal identity for a Kubernetes-native DNS server: a boundary stone, a lowercase wordmark, and a small palette that works in documentation and a dashboard.

**Status:** proposed direction. This package contains a brief and [image-generation prompts](prompts.md); no logo images have been generated.

## Product and audience

kudurru combines authoritative DNS, resolution, blocking, and multi-node operation in one Go binary. It runs in Kubernetes or a Docker container. Its audience includes infrastructure engineers, platform teams, and home-lab operators.

The name comes from Babylonian boundary stones that recorded land grants and authority over territory. DNS zones likewise record authority over a defined space. That connection should make the identity distinctive without requiring a history lesson.

Write the name **kudurru**, lowercase, including in the wordmark. Pronunciation: **koo-DOO-roo**. Use **Kubernetes-native DNS** as an optional descriptor in surrounding copy, rather than inside the core logo. The visual identity must not imply that the pre-alpha project is production-ready.

## Recommended direction: Boundary stone

Use a compact, upright stone silhouette with subtly angled shoulders and a flat base. A broad, angular **k** cut into its center supplies the memorable detail. The cutout should be simple enough to survive small display sizes, with ample solid material around it.

Treat the stone as a graphic symbol: flat, precise, and calm. Its connection to antiquity comes from its outline, not weathering, invented inscriptions, or archaeological illustration. The identity should feel dependable and contemporary.

Pair the mark with a clean, medium-weight lowercase wordmark. The horizontal lockup is the default for the repository and documentation; the standalone mark is for avatars, the dashboard sidebar, and app icons.

## Palette

| Token | Color | Intended use |
| --- | --- | --- |
| `brand.ink` | `#182522` | Primary mark, wordmark, body text, dark surfaces |
| `brand.clay` | `#B65C3A` | Restrained accent; optional mark on light surfaces |
| `brand.paper` | `#F7F3EA` | Warm light background; reversed mark on dark surfaces |
| `brand.stone` | `#D9D1C2` | Dividers and secondary surfaces |
| `brand.white` | `#FFFFFF` | Neutral background and export matte |

The default logo is one color: Ink on a light surface, or Paper on Ink. Clay is an accent, not a required second color in the mark. Keep application success, warning, blocked, and error colors separate from this brand palette.

Ink on Paper has approximately **14.3:1** contrast; Clay on Paper has approximately **4.1:1**. Use Ink for normal-sized text on Paper. These are proposed digital colors; inspect the generated artwork and normalize export colors before treating it as a master asset.

## Typography

- **Wordmark direction:** a geometric sans serif with open counters, medium weight, and deliberate spacing. The generated wordmark is artwork; do not assume it is an exact font specimen.
- **Documentation and future UI:** Inter, with `ui-sans-serif, system-ui, sans-serif` as fallbacks.
- **Commands and DNS records:** JetBrains Mono, with `ui-monospace, SFMono-Regular, Consolas, monospace` as fallbacks.

No font files or external font dependencies are added by this brief. If fonts are bundled later, include their license notices and serve them locally, consistent with the dashboard's planned no-CDN policy.

## Planned minimal kit

These are future deliverables, not files currently present. Keep final assets under `docs/brand/assets/` so documentation and the future frontend can share a stable source.

| Planned file | Specification | Use |
| --- | --- | --- |
| `kudurru-mark-ink.png` | Transparent PNG, 1024 × 1024 | Source for avatars and icons |
| `kudurru-mark-paper.png` | Same geometry and alpha, Paper foreground | Dark backgrounds |
| `kudurru-logo-ink.png` | Transparent horizontal lockup, 1800 px wide | README and light documentation |
| `kudurru-logo-paper.png` | Same lockup and alpha, Paper foreground | Dark documentation and dashboard |
| `kudurru-icon-192.png`, `kudurru-icon-512.png` | Square PNG, Ink tile with Paper mark | Application icons; mark inset by 20% |
| `favicon.ico` | 16, 32, and 48 px frames | Browser tabs; inspect every size |
| `kudurru-social.png` | 1200 × 630 PNG, Ink background | Repository social preview |
| `brand-sheet.png` | Contact sheet of approved exports and swatches | Quick visual reference |

Keep original generated outputs and their exact prompts in `docs/brand/sources/` when generation happens. The generated mark and lockup should be the source for every export. Recolor, resize, and compose derivatives from those approved images to preserve geometry. Do not generate a new symbol independently for each size.

## Usage rules

- Reserve clear space around the artwork of at least one quarter of the mark's visible height. Preserve that spacing inside export canvases.
- Aim for a minimum mark height of 24 px and a horizontal lockup width of 140 px. These are starting targets; confirm them against the generated result. Inspect favicon versions separately at actual size.
- Use the Ink version on Paper or White. Use the Paper version on Ink. On photography or a busy surface, give the logo a solid backing.
- Keep the aspect ratio, cutout, and spacing intact. Avoid outlines, shadows, gradients, texture, or additional decorative symbols.
- For an image accompanied by a visible project title, use empty alt text. For a standalone logo that identifies the project, use `alt="kudurru"`.

## Generation and acceptance

Start with the [standalone mark prompt](prompts.md#1-primary-mark), approve its geometry, then use that image as a reference for the [horizontal lockup](prompts.md#2-horizontal-lockup). Produce reversed variants and icons by deterministic image processing. The social preview can be composed from the approved lockup or generated with it as a reference using [prompt 3](prompts.md#3-repository-social-preview).

Before adopting the kit, inspect the mark at 16, 24, and 48 px; verify that the wordmark reads exactly `kudurru`; compare mark geometry between exports; and check transparent edges on both light and dark surfaces. Confirm that there are no stray pixels, checkerboard patterns, or baked-in backgrounds in transparent files. A generated PNG is raster artwork, not an editable vector master.

If the primary stone direction is visually weak, use the [optional alternative prompt](prompts.md#4-optional-alternative-zone-seal) to explore a zone seal before creating derivatives. Adopt one direction for the entire kit.
