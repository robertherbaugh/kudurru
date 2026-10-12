"""Derives the dashboard's brand images from docs/brand/assets.

The originals stay in docs/brand; the dashboard embeds downscaled copies
(twice the displayed CSS size) so the Go binary grows by tens of KiB, not
by the full-resolution exports. Run from the repository root:

    python3 -m venv .context/brand-venv
    .context/brand-venv/bin/pip install Pillow==11.3.0
    .context/brand-venv/bin/python3 web/scripts/brand-assets.py
"""
import shutil
from pathlib import Path

from PIL import Image

SRC = Path("docs/brand/assets")
ASSETS = Path("web/src/assets")  # imported by the SPA, hashed into dist/assets
PUBLIC = Path("web/public")  # copied to the dist root under stable names
BLOCKPAGE = Path("internal/blockpage/assets")  # embedded by the block page

# name -> (width, height) in pixels: 2x of the CSS size in styles.css
SCALED = {
    "kudurru-logo-mixed.png": (360, 120),  # header, light (.k-logo 180x60)
    "kudurru-logo-mixed-paper.png": (360, 120),  # header, dark
    "kudurru-logo-mixed-tagline.png": (720, 300),  # sign-in page, light (360x150)
    "kudurru-logo-mixed-paper-tagline.png": (720, 300),  # sign-in page, dark
}

# copied unchanged: browser tab icon and the web app manifest icons
COPIED = {
    "favicon.ico": "favicon.ico",
    "kudurru-icon-192.png": "icon-192.png",
    "kudurru-icon-512.png": "icon-512.png",
}


def main() -> None:
    ASSETS.mkdir(parents=True, exist_ok=True)
    PUBLIC.mkdir(parents=True, exist_ok=True)
    for name, size in SCALED.items():
        im = Image.open(SRC / name).convert("RGBA")
        im = im.resize(size, Image.Resampling.LANCZOS)
        # A 64-colour palette keeps the antialiased edges of the three brand
        # colours and makes the files several times smaller than RGBA.
        im = im.quantize(colors=64, method=Image.Quantize.FASTOCTREE, dither=Image.Dither.NONE)
        im.save(ASSETS / name, optimize=True)
    for src, dst in COPIED.items():
        shutil.copyfile(SRC / src, PUBLIC / dst)
    # The block page inlines the header logos and serves the favicon.
    BLOCKPAGE.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(ASSETS / "kudurru-logo-mixed.png", BLOCKPAGE / "logo-light.png")
    shutil.copyfile(ASSETS / "kudurru-logo-mixed-paper.png", BLOCKPAGE / "logo-dark.png")
    shutil.copyfile(SRC / "favicon.ico", BLOCKPAGE / "favicon.ico")


if __name__ == "__main__":
    main()
