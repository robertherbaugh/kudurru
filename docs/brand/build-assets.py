#!/usr/bin/env python3
"""Export the kudurru kit from supplied PNGs. Requires Pillow 11.3.0."""

import hashlib
import json
from pathlib import Path

from PIL import Image, ImageDraw, ImageFilter, ImageFont, ImageOps


ROOT = Path(__file__).resolve().parent
SOURCES = ROOT / "sources"
ASSETS = ROOT / "assets"
COLORS = {
    "ink": "#182522",
    "clay": "#B65C3A",
    "paper": "#F7F3EA",
    "stone": "#D9D1C2",
    "white": "#FFFFFF",
}
MARK_SOURCE = "mixed/kudurru-mark-clay.png"
LOGO_SOURCE = "mixed/kudurru-logo-mixed.png"
TAGLINE_SOURCE = "ink/kudurru-logo-ink-tagline.png"
RESAMPLE = Image.Resampling.LANCZOS


def clean_mask(source):
    """Remove faint alpha noise and isolated pixels, retaining soft edges."""
    alpha = Image.open(SOURCES / source).convert("RGBA").getchannel("A")
    alpha = alpha.point(
        lambda value: max(0, min(255, round((value - 32) * 255 / 192)))
    )
    return alpha.filter(ImageFilter.MedianFilter(3))


def foreground(mask, color):
    result = Image.new("RGBA", mask.size, COLORS[color])
    result.putalpha(mask)
    return result


def crop(image):
    return image.crop(image.getchannel("A").getbbox())


def contain(image, size):
    return ImageOps.contain(image, size, RESAMPLE)


def centered(image, canvas_size, artwork_size, background=None):
    canvas = Image.new("RGBA", canvas_size, background or (0, 0, 0, 0))
    artwork = contain(image, artwork_size)
    canvas.alpha_composite(
        artwork,
        ((canvas.width - artwork.width) // 2, (canvas.height - artwork.height) // 2),
    )
    return canvas


def label_font(size):
    # Used only for labels on the contact sheet, never for the supplied wordmark.
    return ImageFont.load_default(size=size)


def main():
    ASSETS.mkdir(exist_ok=True)
    mark_mask = clean_mask(MARK_SOURCE)
    logo_mask = clean_mask(LOGO_SOURCE)
    tagline_mask = clean_mask(TAGLINE_SOURCE)
    tagline_mask = tagline_mask.crop((0, 590, tagline_mask.width, 680))
    taglines = {
        color: crop(foreground(tagline_mask, color)) for color in ("ink", "paper")
    }
    marks = {color: crop(foreground(mark_mask, color)) for color in ("ink", "clay", "paper")}
    logos = {}
    for variant, wordmark in (
        ("ink", "ink"), ("clay", "clay"), ("paper", "paper"),
        ("mixed", "ink"), ("mixed-paper", "paper"),
    ):
        logo = foreground(logo_mask, wordmark)
        if variant.startswith("mixed"):
            # The supplied mixed lockup's mark ends before x=450; text starts after it.
            logo.paste(COLORS["clay"], (0, 0, 450, logo.height))
            logo.putalpha(logo_mask)
        logos[variant] = crop(logo)
        centered(logos[variant], (1536, 512), (1312, 320)).save(
            ASSETS / f"kudurru-logo-{variant}.png", optimize=True
        )

    for color, mark in marks.items():
        centered(mark, (1024, 1024), (680, 680)).save(
            ASSETS / f"kudurru-mark-{color}.png", optimize=True
        )

    for variant, text_color in (("mixed", "ink"), ("mixed-paper", "paper")):
        canvas = Image.new("RGBA", (1536, 640))
        artwork = contain(logos[variant], (1312, 320))
        tagline = contain(taglines[text_color], (880, 70))
        group_height = artwork.height + 36 + tagline.height
        top = (canvas.height - group_height) // 2
        canvas.alpha_composite(artwork, ((canvas.width - artwork.width) // 2, top))
        canvas.alpha_composite(
            tagline,
            ((canvas.width - tagline.width) // 2, top + artwork.height + 36),
        )
        canvas.save(ASSETS / f"kudurru-logo-{variant}-tagline.png", optimize=True)

    for size in (192, 512):
        centered(marks["clay"], (size, size), (round(size * .6),) * 2, COLORS["ink"]).convert("RGB").save(
            ASSETS / f"kudurru-icon-{size}.png", optimize=True
        )
    favicon = centered(marks["clay"], (48, 48), (42, 42), COLORS["ink"])
    favicon.save(ASSETS / "favicon.ico", sizes=[(16, 16), (32, 32), (48, 48)])

    social_logo = Image.open(ASSETS / "kudurru-logo-mixed-paper-tagline.png").convert("RGBA")
    centered(crop(social_logo), (1200, 630), (1020, 380), COLORS["ink"]).convert("RGB").save(
        ASSETS / "kudurru-social.png", optimize=True
    )

    sheet = Image.new("RGBA", (1440, 1240), COLORS["paper"])
    draw = ImageDraw.Draw(sheet)
    draw.text((56, 32), "kudurru / minimal branding kit", font=label_font(32), fill=COLORS["ink"])
    samples = [
        ("mixed", "paper", "Primary / light"),
        ("mixed-paper", "ink", "Primary / dark"),
        ("ink", "white", "Ink / monochrome"),
        ("paper", "ink", "Paper / monochrome"),
        ("clay", "paper", "Clay / accent"),
        ("mixed-paper-tagline", "ink", "With supplied tagline"),
    ]
    for index, (variant, background, label) in enumerate(samples):
        x, y = 56 + index % 2 * 672, 100 + index // 2 * 230
        tile = Image.new("RGBA", (656, 214), COLORS[background])
        logo = Image.open(ASSETS / f"kudurru-logo-{variant}.png").convert("RGBA")
        tile.alpha_composite(centered(crop(logo), tile.size, (540, 132)))
        td = ImageDraw.Draw(tile)
        td.text((18, 16), label, font=label_font(16), fill=COLORS["paper" if background == "ink" else "ink"])
        sheet.alpha_composite(tile, (x, y))
    draw.text((56, 814), "Marks and application icon", font=label_font(22), fill=COLORS["ink"])
    for index, color in enumerate(("ink", "clay", "paper")):
        tile = centered(marks[color], (150, 150), (98, 98), COLORS["ink" if color == "paper" else "white"])
        sheet.alpha_composite(tile, (56 + index * 175, 855))
    icon = Image.open(ASSETS / "kudurru-icon-192.png").convert("RGBA").resize((150, 150), RESAMPLE)
    sheet.alpha_composite(icon, (581, 855))
    draw.text((770, 862), "authority over every zone.", font=label_font(24), fill=COLORS["ink"])
    draw.text((770, 902), "kudurru / koo-DOO-roo", font=label_font(18), fill=COLORS["ink"])
    draw.text((770, 944), "Transparent PNGs + app icons + favicon", font=label_font(18), fill=COLORS["ink"])
    for index, (name, color) in enumerate(COLORS.items()):
        x = 56 + index * 268
        draw.rectangle((x, 1040, x + 252, 1140), fill=color, outline=COLORS["stone"], width=1)
        draw.text((x, 1155), f"{name} / {color}", font=label_font(17), fill=COLORS["ink"])
    sheet.convert("RGB").save(ASSETS / "kudurru-brand-sheet.png", optimize=True)

    tokens = {"colors": COLORS, "fontFamily": {"sans": "Inter, ui-sans-serif, system-ui, sans-serif", "mono": "JetBrains Mono, ui-monospace, SFMono-Regular, Consolas, monospace"}}
    (ROOT / "tokens.json").write_text(json.dumps(tokens, indent=2) + "\n")
    declarations = [f"  --kudurru-{name}: {value};" for name, value in COLORS.items()]
    declarations += [f"  --kudurru-font-{name}: {value};" for name, value in tokens["fontFamily"].items()]
    (ROOT / "tokens.css").write_text(":root {\n" + "\n".join(declarations) + "\n}\n")
    entries = []
    for path in sorted(ASSETS.iterdir()):
        if path.suffix not in (".png", ".ico"):
            continue
        with Image.open(path) as image:
            entry = {"file": path.name, "width": image.width, "height": image.height, "mode": image.mode, "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
            if path.suffix == ".ico":
                entry["sizes"] = sorted([list(size) for size in image.ico.sizes()])
        entries.append(entry)
    manifest = {"sourceTemplates": [MARK_SOURCE, LOGO_SOURCE, TAGLINE_SOURCE], "processing": "Alpha values 0..32 removed; 32..224 remapped to 0..255; median 3x3 filter; exact palette recoloring; aspect-preserving resizing and composition. All source PNG bytes remain unchanged.", "pillowVersion": "11.3.0", "files": entries}
    (ASSETS / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Exported {len(entries)} images, color tokens, and an asset manifest.")


if __name__ == "__main__":
    main()
