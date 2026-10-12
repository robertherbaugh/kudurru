// A small markdown renderer for exactly what the PolyForm license text uses
// (headings, paragraphs, a block quote, bold, bold italic, code, links and
// autolinks), so kudurru.io shows the repository's LICENSE file as it is,
// from one source, without a markdown dependency. Anything else in the input
// is rendered as plain text, never as markup: the text is escaped first.
import { escape, raw, type Html } from "./html.ts";

/** GitHub's heading id: lowercase, punctuation dropped, spaces to hyphens. */
export function slug(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9 -]/g, "")
    .trim()
    .replace(/ /g, "-");
}

function inline(text: string, base: string): string {
  let out = escape(text);
  out = out.replace(/`([^`]+)`/g, "<code>$1</code>");
  out = out.replace(/\*\*\*(.+?)\*\*\*/g, "<strong><em>$1</em></strong>");
  out = out.replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>");
  // [text](#anchor) stays on the page it is rendered on; [text](https://…) leaves it.
  out = out.replace(/\[([^\]]+)\]\(#([^)]+)\)/g, (_m, t: string, a: string) => `<a href="${base}#${a}">${t}</a>`);
  out = out.replace(/\[([^\]]+)\]\((https:\/\/[^)\s]+)\)/g, '<a href="$2" rel="noopener">$1</a>');
  out = out.replace(/&lt;(https:\/\/[^\s&]+)&gt;/g, '<a href="$1" rel="noopener">$1</a>');
  return out;
}

export interface Rendered {
  /** The text of the first h1, or "". */
  title: string;
  /** The page body without the h1. */
  body: Html;
  /** The ids of the h2 headings, in order. */
  ids: string[];
}

/** Renders markdown; `base` is the path the anchors are made absolute against. */
export function renderMarkdown(source: string, base: string): Rendered {
  let title = "";
  const ids: string[] = [];
  const parts: string[] = [];
  for (const block of source.replace(/\r\n/g, "\n").split(/\n{2,}/)) {
    const text = block.trim();
    if (!text) continue;
    const h = /^(#{1,2}) (.+)$/.exec(text);
    if (h) {
      if (h[1] === "#") {
        title ||= h[2]!;
        continue;
      }
      const id = slug(h[2]!);
      ids.push(id);
      parts.push(`<h2 id="${id}">${inline(h[2]!, base)}</h2>`);
    } else if (text.startsWith(">")) {
      const quoted = text.split("\n").map((l) => l.replace(/^>\s?/, "")).join(" ");
      parts.push(`<blockquote><p>${inline(quoted, base)}</p></blockquote>`);
    } else {
      parts.push(`<p>${inline(text.replace(/\n/g, " "), base)}</p>`);
    }
  }
  return { title, body: raw(parts.join("\n")), ids };
}
