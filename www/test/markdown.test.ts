import { describe, expect, test } from "bun:test";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { renderMarkdown, slug } from "../src/markdown.ts";

const BASE = "/license/text/";

describe("renderMarkdown", () => {
  test("headings get GitHub ids, the h1 becomes the title", () => {
    const r = renderMarkdown("# The Title\n\n## Changes and New Works License\n\ntext", BASE);
    expect(r.title).toBe("The Title");
    expect(r.ids).toEqual(["changes-and-new-works-license"]);
    expect(r.body.value).toContain('<h2 id="changes-and-new-works-license">Changes and New Works License</h2>');
    expect(r.body.value).not.toContain("<h1");
    expect(slug("No Other Rights")).toBe("no-other-rights");
  });

  test("anchors stay on the page, links leave it, markup in the text is escaped", () => {
    const r = renderMarkdown("See [Distribution License](#distribution-license), <https://polyformproject.org/x> and [a](https://example.org/a).\n\n<script>alert(1)</script> & `Required Notice:`", BASE);
    expect(r.body.value).toContain('<a href="/license/text/#distribution-license">Distribution License</a>');
    expect(r.body.value).toContain('<a href="https://polyformproject.org/x" rel="noopener">https://polyformproject.org/x</a>');
    expect(r.body.value).toContain('<a href="https://example.org/a" rel="noopener">a</a>');
    expect(r.body.value).toContain("&lt;script&gt;alert(1)&lt;/script&gt; &amp; <code>Required Notice:</code>");
    expect(r.body.value).not.toContain("<script>");
  });

  test("emphasis and block quotes", () => {
    const r = renderMarkdown("***As is.*** The **licensor** is you.\n\n> Required Notice: Copyright Yoyodyne\n> (http://example.com)", BASE);
    expect(r.body.value).toContain("<strong><em>As is.</em></strong> The <strong>licensor</strong> is you.");
    expect(r.body.value).toContain("<blockquote><p>Required Notice: Copyright Yoyodyne (http://example.com)</p></blockquote>");
  });
});

describe("the repository's LICENSE", () => {
  test("renders with every section, working anchors and all of its words", async () => {
    const source = await readFile(join(import.meta.dir, "../../LICENSE"), "utf8");
    const r = renderMarkdown(source, BASE);
    expect(r.title).toBe("PolyForm Noncommercial License 1.0.0");
    expect(r.ids).toEqual([
      "acceptance", "copyright-license", "distribution-license", "notices", "changes-and-new-works-license", "patent-license",
      "noncommercial-purposes", "personal-uses", "noncommercial-organizations", "fair-use", "no-other-rights", "patent-defense",
      "violations", "no-liability", "definitions",
    ]);
    // Every in-page link has a target.
    for (const m of r.body.value.matchAll(/href="\/license\/text\/#([^"]+)"/g)) expect(r.ids).toContain(m[1]!);
    // No word of the license is lost: compare the visible text with the source.
    const decode = (s: string) => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
    const words = (s: string) => decode(s.replace(/<[^>]+>/g, " ")).replace(/[`*#>\[\]()<]/g, " ").replace(/https?:\/\/\S+/g, "").split(/\s+/).filter(Boolean);
    const shown = words(r.body.value);
    const wanted = words(source.replace(/^# .*$/m, "").replace(/\(#[a-z-]+\)/g, ""));
    expect(shown.join(" ")).toBe(wanted.join(" "));
  });
});
