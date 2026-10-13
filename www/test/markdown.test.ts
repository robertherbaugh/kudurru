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

  test("eligibility lists, redistribution steps and entitlement tables render safely", () => {
    const r = renderMarkdown("- Eligible nonprofit\n- <script>bad</script>\n\n1. Keep notices\n2. Share source\n\n| Capability | No key |\n|---|---|\n| `DNS` | Yes |\n| <img src=x> | No |", BASE);
    expect(r.body.value).toContain("<ul><li>Eligible nonprofit</li><li>&lt;script&gt;bad&lt;/script&gt;</li></ul>");
    expect(r.body.value).toContain("<ol><li>Keep notices</li><li>Share source</li></ol>");
    expect(r.body.value).toContain('<th scope="col">Capability</th>');
    expect(r.body.value).toContain("<td><code>DNS</code></td><td>Yes</td>");
    expect(r.body.value).toContain("<td>&lt;img src=x&gt;</td><td>No</td>");
    expect(r.body.value).not.toContain("<script>");
    expect(r.body.value).not.toContain("<img");
  });
});

describe("the repository's LICENSE", () => {
  test("renders with every section, working anchors and all of its words", async () => {
    const source = await readFile(join(import.meta.dir, "../../LICENSE"), "utf8");
    const r = renderMarkdown(source, BASE);
    expect(r.title).toBe("Kudurru Community License");
    expect(r.ids).toEqual([
      "1-parties-software-and-acceptance", "2-permitted-use-categories", "3-commercial-and-government-use-requires-separate-authorization",
      "4-copyright-and-modification-rights", "5-redistribution-forks-and-notices", "6-license-keys-and-feature-entitlements",
      "7-safety-of-dns-operations-and-key-expiration", "8-community-contributions", "9-patent-permissions-and-patent-disputes",
      "10-ownership-and-trademarks", "11-breach-termination-and-cure", "12-third-party-components-and-separate-licenses",
      "13-disclaimer-of-warranties-and-limitation-of-liability", "14-general-terms",
    ]);
    // Every in-page link has a target.
    for (const m of r.body.value.matchAll(/href="\/license\/text\/#([^"]+)"/g)) expect(r.ids).toContain(m[1]!);
    // No word of the license is lost: compare the visible text with the source.
    const decode = (s: string) => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
    const words = (s: string) => decode(s.replace(/<[^>]+>/g, " ")).replace(/[`*#>\[\]()<]/g, " ").replace(/https?:\/\/\S+/g, "").split(/\s+/).filter(Boolean);
    const shown = words(r.body.value);
    const wanted = words(source.replace(/^# .*$/m, "").replace(/^\|(?:\s*:?-+:?\s*\|)+\s*$/gm, "").replace(/^(?:- |\d+\. )/gm, "").replace(/\|/g, " ").replace(/\(#[a-z-]+\)/g, ""));
    expect(shown.join(" ")).toBe(wanted.join(" "));
  });
});
