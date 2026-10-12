# kudurru image-generation prompts

These prompts preserve the initial branding brief for possible future image generation. They were not run in this workspace and are not the known provenance of the supplied logos. Use the current [branding kit](README.md) and its supplied sources as the visual reference for any future work. Descriptions such as “vector-like” refer to the visual style, not the file format.

For a new exploration, generate the primary mark first and use its approved output as a reference for subsequent compositions. To extend the current identity, supply an existing kit asset instead. A reference image should be supplied through the generation tool's image input; merely naming a file in a prompt does not attach it.

## Background and export settings

With a built-in image-generation tool, request genuine transparency for the primary mark and horizontal lockup. Preserve the resulting alpha channel.

If the API/CLI fallback is explicitly selected later, the imagegen skill's default model is `gpt-image-2`, which does not support a transparent background setting. For the mark and lockup, replace the `Scene/backdrop` line with:

> Solid pure white (#FFFFFF), completely uniform. No shadow, paper texture, checkerboard, or off-white pixels in the background. Keep the foreground a single solid dark color and all edges clean, suitable for later background removal.

Generate at high quality. Suggested generation sizes are 1024 × 1024 for the mark, 1536 × 1024 for the lockup, and 1536 × 1024 for the social composition. Crop, pad, or resample to the final export dimensions in the [kit](README.md#asset-inventory) afterward. Remove a white matte with edge decontamination before making transparent exports; do not label an opaque image as transparent. Do not switch models to obtain transparency without explicit confirmation.

## 1. Primary mark

Suggested source filename: `sources/boundary-stone-mark.png`.

```text
Use case: logo-brand
Asset type: standalone logo mark for an open-source DNS server; square PNG master
Primary request: Create an original minimal logo symbol for kudurru, a Kubernetes-native DNS server named after Babylonian boundary stones that recorded authority over territory.
Scene/backdrop: Genuinely transparent background with an alpha channel.
Subject: One compact upright boundary-stone silhouette with subtly angled shoulders and a flat base. A broad angular lowercase-k-inspired cutout sits inside the stone. The k is negative space, not a separate letter placed over the symbol. Keep the silhouette and cutout bold, balanced, and recognizable at small sizes.
Style/medium: Flat, vector-like graphic design rendered as a raster image. One solid foreground color. Crisp edges and balanced negative space. Contemporary infrastructure branding with a restrained reference to a boundary stone.
Composition/framing: One centered symbol on a square canvas. Occupy approximately 60% of the canvas height. Equal optical margins. No presentation sheet or multiple options in one image.
Color palette: Foreground Ink #182522 only; transparent cutout and background.
Text: None. No project name, captions, or descriptors.
Constraints: Original design. Few broad shapes. The central cutout must remain legible at 24 pixels. The stone should feel compact rather than tall and slender. No enclosed tiny counters or hairline details.
Avoid: Photorealistic stone, erosion, cuneiform, invented historical inscriptions, gradients, shadows, 3D, glows, textures, borders, badges, generic shields, globes, network-node diagrams, Kubernetes emblems, existing logos, watermark, checkerboard transparency simulation.
```

## 2. Horizontal lockup

Suggested source filename: `sources/boundary-stone-lockup.png`.

Required image input: the approved primary mark from prompt 1. This is a reference for exact symbol geometry, not an invitation to redesign it.

```text
Use case: logo-brand
Asset type: horizontal logo lockup for repository documentation and a dashboard header; PNG master
Primary request: Pair the supplied kudurru boundary-stone mark with a polished lowercase wordmark reading exactly "kudurru".
Input images: Image 1 is the approved primary mark. Preserve its silhouette, central cutout, proportions, orientation, and visual weight exactly.
Scene/backdrop: Genuinely transparent background with an alpha channel.
Subject: The supplied symbol followed by the lowercase wordmark kudurru. Use a clean geometric sans serif, medium weight, with open counters and carefully balanced spacing. The wordmark should feel calm and technically precise.
Composition/framing: One horizontal lockup, symbol to the left and wordmark to the right. Align them optically. Make the wordmark's visible letter height roughly 60% of the symbol height. Leave a gap approximately one third of the symbol height between symbol and text. Center the entire lockup with generous clear space.
Color palette: Symbol and wordmark in one solid Ink #182522; transparent cutout and background.
Text (verbatim): "kudurru". Exactly seven lowercase letters: k u d u r r u. No other text.
Constraints: Preserve the input mark exactly. Do not turn the wordmark's k into a second illustrated symbol. All letters must be readable and evenly spaced. Flat raster artwork with crisp edges.
Avoid: Extra letters, uppercase letters, slogans, descriptors, decorative ligatures that obscure spelling, additional symbols, multiple lockups, gradients, shadows, textures, mockups, watermark, checkerboard transparency simulation.
```

## 3. Repository social preview

Suggested source filename: `sources/boundary-stone-social.png`.

Required image input: the approved horizontal lockup from prompt 2. Prefer composing this card directly from that lockup when exact fidelity is required.

```text
Use case: logo-brand
Asset type: minimal GitHub repository social preview; landscape raster image
Primary request: Create a restrained social preview for the open-source project kudurru using the supplied approved logo lockup.
Input images: Image 1 is the approved kudurru horizontal lockup. Preserve its symbol geometry, exact seven-letter wordmark, letterforms, spacing, and proportions. Recolor its foreground only.
Scene/backdrop: Completely flat solid Ink #182522 background.
Subject: The approved horizontal logo lockup in Paper #F7F3EA, with the descriptor "Kubernetes-native DNS" beneath it in a clean sans serif. The identity is a Babylonian boundary stone interpreted as a modern infrastructure symbol.
Composition/framing: A centered typographic composition with ample negative space. Keep the lockup and descriptor within the central 70% of the canvas width and 60% of its height so it can be cropped to a 1200-by-630 card without clipping. The descriptor is clearly secondary to the logo.
Color palette: Ink #182522 background; Paper #F7F3EA logo and descriptor. No additional colors needed.
Text (verbatim): The supplied wordmark "kudurru" and the descriptor "Kubernetes-native DNS". No other copy.
Constraints: Preserve the approved logo. Keep all text readable. Original flat raster artwork. Use a simple composition that remains clear in a small repository preview.
Avoid: Product screenshots, feature lists, production-readiness claims, URL text, decorative networks, extra symbols, historical illustrations, gradients, glows, shadows, texture, 3D, watermark.
```

## 4. Optional alternative: Zone seal

Use this only if the boundary-stone direction does not produce a strong mark. It is a separate exploration, not a second simultaneous identity.

Suggested source filename: `sources/zone-seal-exploration.png`.

```text
Use case: logo-brand
Asset type: alternative standalone logo mark for kudurru; square PNG
Primary request: Create an original minimal symbol for kudurru, an open-source DNS server whose name refers to Babylonian boundary stones and authority over a defined zone.
Scene/backdrop: Genuinely transparent background with an alpha channel.
Subject: A compact abstract zone seal. Two bold opposing bracket-like forms define a bounded space, with one short broad horizontal stroke inside representing a record. Unify the shapes into one balanced emblem with strong negative space and a memorable silhouette.
Style/medium: Flat geometric graphic design rendered as a raster image. Precise, calm, and contemporary. One solid foreground color. No historical ornament.
Composition/framing: One centered mark, approximately 60% of square canvas height, with generous equal margins. No wordmark or presentation sheet.
Color palette: Ink #182522 foreground only; transparent background.
Text: None.
Constraints: Original design. Few bold shapes, no hairlines, no tiny details. Legible at 24 pixels. Suggest a defined zone and a record without drawing a literal network or database.
Avoid: Shields, padlocks, globes, Kubernetes emblems, existing logos, cuneiform, textures, gradients, shadows, 3D, captions, watermark, checkerboard transparency simulation.
```

## Derivatives that need no new generation

Create Paper and optional Clay variants by recoloring the approved foreground while preserving alpha and geometry. Build square app tiles, favicon frames, and the brand sheet from those assets. Preserve original generated files before processing. Record the tool, model, exact prompt, input references, and processing steps alongside the source files when generation happens.
