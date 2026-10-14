"""Export the email's bundled brand assets (requires Pillow, as docs/brand does).

Run from the repository root:
  python3 license-server/scripts/mail-brand.py
The JSON is embedded in the server bundle, so sending mail needs no asset files
or image host at runtime. Keep the original logo's aspect ratio and clear space.
"""

import base64
import io
import json
from pathlib import Path

from PIL import Image

root = Path(__file__).resolve().parents[2]
brand = root / "docs/brand"
tokens = json.loads((brand / "tokens.json").read_text())
logo = Image.open(brand / "assets/kudurru-logo-mixed-paper.png")
logo = logo.resize((576, 192), Image.Resampling.LANCZOS)
png = io.BytesIO()
logo.save(png, format="PNG", optimize=True)
output = {"colors": tokens["colors"], "logo": base64.b64encode(png.getvalue()).decode("ascii")}
(root / "license-server/server/mail/brand.json").write_text(json.dumps(output, indent=2) + "\n")
