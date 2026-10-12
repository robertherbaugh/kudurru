#!/usr/bin/env python3
"""Checks docs/guide/ against the writing rules of docs/guide/style.md.

    python3 hack/guide-lint.py            # the checks that must pass
    python3 hack/guide-lint.py --review   # also list passive voice and -ing forms

This is not an ASD-STE100 validator: it does not know the approved
dictionary. It finds what a program can find: long sentences, long
paragraphs, contractions, words that the guide does not use, broken
links and anchors, missing images and figure numbers out of order.
Standard library only.
"""
import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GUIDE = ROOT / "docs" / "guide"

PROCEDURE_LIMIT = 20  # words in a sentence of a numbered step
OTHER_LIMIT = 25  # words in any other sentence
PARAGRAPH_LIMIT = 6  # sentences in a paragraph

# Words and phrases that the guide does not use (see style.md).
BANNED = [
    r"should", r"may", r"via", r"e\.g\.", r"i\.e\.", r"etc\.?", r"please",
    r"ensure", r"utili[sz]e", r"simply", r"just", r"easily", r"in order to",
    r"and/or", r"will", r"let's", r"shall",
]
BANNED_RE = re.compile(r"(?<![\w-])(" + "|".join(BANNED) + r")(?![\w-])", re.I)
CONTRACTION_RE = re.compile(
    r"\b\w+n[’']t\b|\b(it|that|there|here|what|who|he|she|we|they|you|I)[’'](s|re|ll|ve|d|m)\b", re.I
)

# Review mode: passive voice and -ing forms (heuristics, many false positives).
PASSIVE_RE = re.compile(r"\b(is|are|was|were|be|been|being)\s+(\w+(?:ed|en)|made|shown|done|set|kept|given|read|run|sent|written|used)\b", re.I)
ING_OK = {
    "string", "thing", "nothing", "something", "anything", "everything", "during", "morning", "evening",
    "ring", "bring", "spring", "king", "building", "following", "missing", "warning", "setting", "settings",
    "blocking", "filtering", "forwarding", "routing", "logging", "naming", "encoding", "listing", "heading",
    "meeting", "training", "pending", "existing", "wizard", "ping", "string", "tracking", "caching",
    "scheduling", "matching", "streaming", "gaming", "messaging", "shopping", "viewing", "monitoring",
    "ongoing", "boarding", "recording", "loading", "sizing", "mapping", "parsing", "tuning",
}
ING_RE = re.compile(r"\b[A-Za-z]{3,}ing\b")

HEADING_RE = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
FENCE_RE = re.compile(r"^\s*(```|~~~)")
LIST_RE = re.compile(r"^(\s*)([-*+]|\d+\.)\s+(.*)$")
TABLE_RE = re.compile(r"^\s*\|.*\|\s*$")
TABLE_SEP_RE = re.compile(r"^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$")
IMAGE_RE = re.compile(r"!\[([^\]]*)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
LINK_RE = re.compile(r"(?<!!)\[([^\]]*)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
CAPTION_RE = re.compile(r"^\*Figure (\d+)\. .+\*$")


def slug(title: str) -> str:
    """GitHub's heading anchor: lower case, no punctuation, spaces to dashes."""
    t = re.sub(r"`([^`]*)`", r"\1", title)
    t = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", t)
    t = re.sub(r"[*_]", "", t).strip().lower()
    t = re.sub(r"[^\w\- ]", "", t)
    return t.replace(" ", "-")


def anchors_of(path: Path) -> set:
    found, seen, fence = set(), {}, False
    for line in path.read_text(encoding="utf-8").splitlines():
        if FENCE_RE.match(line):
            fence = not fence
            continue
        if not fence:
            found.update(re.findall(r'<a id="([^"]+)"', line))
        m = None if fence else HEADING_RE.match(line)
        if m:
            s = slug(m.group(2))
            n = seen.get(s, 0)
            seen[s] = n + 1
            found.add(s if n == 0 else f"{s}-{n}")
    return found


def plain(text: str) -> str:
    """Text as a reader counts it: code is one word, links show their text."""
    t = IMAGE_RE.sub("", text)
    t = re.sub(r"`[^`]*`", "CODE", t)
    t = LINK_RE.sub(r"\1", t)
    t = re.sub(r"<[^>]+>", "", t)
    t = re.sub(r"[*_]{1,3}", "", t)
    return t.strip()


def sentences(text: str) -> list:
    parts = re.split(r"(?<=[.!?])\s+(?=[A-Z0-9\[(]|kudurru\b)", text)
    return [p for p in (s.strip() for s in parts) if p]


def words(sentence: str) -> int:
    return len([w for w in sentence.split() if re.search(r"\w", w)])


class Linter:
    def __init__(self, review: bool):
        self.review = review
        self.errors = []
        self.notes = []
        self.anchor_cache = {}

    def err(self, path, line, msg):
        self.errors.append(f"{path.relative_to(ROOT)}:{line}: {msg}")

    def note(self, path, line, msg):
        self.notes.append(f"{path.relative_to(ROOT)}:{line}: {msg}")

    def anchors(self, p: Path) -> set:
        if p not in self.anchor_cache:
            self.anchor_cache[p] = anchors_of(p)
        return self.anchor_cache[p]

    def blocks(self, path: Path):
        """Yields (line, kind, text): kind is heading, step, item, para or cell."""
        fence, para, para_line, para_kind = False, [], 0, "para"

        def flush():
            nonlocal para, para_line, para_kind
            if para:
                yield (para_line, para_kind, " ".join(para))
            para, para_kind = [], "para"

        for n, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if FENCE_RE.match(raw):
                yield from flush()
                fence = not fence
                continue
            if fence or raw.strip().startswith("<!--"):
                continue
            if not raw.strip():
                yield from flush()
                continue
            h = HEADING_RE.match(raw)
            if h:
                yield from flush()
                yield (n, "heading", h.group(2))
                continue
            if TABLE_RE.match(raw):
                yield from flush()
                if not TABLE_SEP_RE.match(raw):
                    for cell in raw.strip().strip("|").split("|"):
                        if cell.strip():
                            yield (n, "cell", cell.strip())
                continue
            if IMAGE_RE.fullmatch(raw.strip()):
                yield from flush()
                continue
            m = LIST_RE.match(raw)
            if m:
                yield from flush()
                para, para_line = [m.group(3)], n
                para_kind = "step" if m.group(2)[0].isdigit() else "item"
                continue
            if para:
                para.append(raw.strip())
            else:
                para, para_line, para_kind = [raw.strip()], n, "para"
        yield from flush()

    def check_text(self, path: Path, exempt_words: bool):
        figures = []
        for line, kind, text in self.blocks(path):
            shown = plain(text)
            cap = CAPTION_RE.match(text.strip())
            if cap:
                figures.append((line, int(cap.group(1))))
            if not exempt_words:
                # Text in bold is text of the dashboard (for example the label "Who may query").
                unlabelled = plain(re.sub(r"\*\*[^*]+\*\*", "", text))
                for m in BANNED_RE.finditer(unlabelled):
                    self.err(path, line, f"word that this guide does not use: '{m.group(0)}'")
                for m in CONTRACTION_RE.finditer(shown):
                    self.err(path, line, f"contraction: '{m.group(0)}'")
            if kind == "heading":
                continue
            limit = PROCEDURE_LIMIT if kind == "step" else OTHER_LIMIT
            ss = sentences(shown)
            for s in ss:
                n = words(s)
                if n > limit:
                    self.err(path, line, f"{n} words (limit {limit}): {s[:70]}...")
            if kind == "para" and len(ss) > PARAGRAPH_LIMIT and not cap:
                self.err(path, line, f"paragraph has {len(ss)} sentences (limit {PARAGRAPH_LIMIT})")
            if self.review and kind in ("para", "step", "item"):
                for m in PASSIVE_RE.finditer(shown):
                    self.note(path, line, f"passive? '{m.group(0)}'")
                for m in ING_RE.finditer(shown):
                    if m.group(0).lower() not in ING_OK:
                        self.note(path, line, f"-ing form? '{m.group(0)}'")
        for i, (line, num) in enumerate(figures, 1):
            if num != i:
                self.err(path, line, f"figure number {num}, expected {i}")

    def check_links(self, path: Path):
        fence = False
        text = path.read_text(encoding="utf-8")
        for n, line in enumerate(text.splitlines(), 1):
            if FENCE_RE.match(line):
                fence = not fence
                continue
            if fence:
                continue
            line_nocode = re.sub(r"`[^`]*`", "", line)
            for m in IMAGE_RE.finditer(line_nocode):
                alt, target = m.group(1), m.group(2)
                if len(alt.split()) < 3:
                    self.err(path, n, f"image needs a descriptive alternative text: {target}")
                if not (path.parent / target).resolve().is_file():
                    self.err(path, n, f"image file not found: {target}")
            for m in LINK_RE.finditer(line_nocode):
                target = m.group(2)
                if re.match(r"[a-z]+:", target):
                    continue
                file_part, _, frag = target.partition("#")
                dest = path if not file_part else (path.parent / file_part).resolve()
                if not dest.exists():
                    self.err(path, n, f"link target not found: {target}")
                    continue
                if frag and dest.suffix == ".md" and frag not in self.anchors(dest):
                    self.err(path, n, f"anchor not found: {target}")

    def run(self):
        files = sorted(GUIDE.rglob("*.md"))
        if not files:
            print("no files under docs/guide", file=sys.stderr)
            return 2
        for f in files:
            self.check_text(f, exempt_words=(f.name == "style.md"))
            self.check_links(f)
        for line in self.errors:
            print(line)
        if self.review:
            for line in self.notes:
                print("review: " + line)
        print(f"{len(files)} files, {len(self.errors)} errors" + (f", {len(self.notes)} review notes" if self.review else ""))
        return 1 if self.errors else 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--review", action="store_true", help="also list passive voice and -ing forms")
    sys.exit(Linter(ap.parse_args().review).run())
