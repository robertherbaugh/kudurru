import { expect, test } from "bun:test";
import { html, raw } from "../src/html.ts";

test("interpolated text is escaped", () => {
  const name = `<script>alert("x")</script>&'`;
  expect(html`<p title="${name}">${name}</p>`.value).toBe(
    "<p title=\"&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;&amp;&#39;\">&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;&amp;&#39;</p>",
  );
});

test("nested html, raw values and arrays are inserted as they are", () => {
  const items = ["a<b", "c"].map((t) => html`<li>${t}</li>`);
  expect(html`<ul>${items}${raw("<!-- x -->")}</ul>`.value).toBe("<ul><li>a&lt;b</li><li>c</li><!-- x --></ul>");
});

test("false, null and undefined render nothing", () => {
  expect(html`[${false}${null}${undefined}${0}]`.value).toBe("[0]");
});
