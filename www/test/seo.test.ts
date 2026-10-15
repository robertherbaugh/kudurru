import { expect, test } from "bun:test";
import { serializeJsonLd, structuredData } from "../src/seo.ts";

test("JSON-LD stays data even when metadata contains HTML delimiters", () => {
  const input = { title: '</script><script>alert("test")</script>', description: "A & B > C" };
  const serialized = serializeJsonLd(input);
  expect(serialized).not.toContain("<");
  expect(serialized).not.toContain(">");
  expect(serialized).not.toContain("&");
  expect(JSON.parse(serialized)).toEqual(input);

  const data = structuredData({ path: "/", title: input.title, description: input.description }, "1.2.3");
  expect(data).not.toContain("</script>");
  expect(JSON.parse(data)["@graph"].find((item: { "@type": string }) => item["@type"] === "WebPage").name).toBe(input.title);
});

test("an explicit breadcrumb label is independent of title formatting", () => {
  const data = structuredData({ path: "/kubernetes-dns/", title: "A new title — kudurru", breadcrumb: "Kubernetes DNS" }, "1.2.3");
  const graph = JSON.parse(data)["@graph"] as { "@type": string; itemListElement?: { name: string }[] }[];
  expect(graph.find((item) => item["@type"] === "BreadcrumbList")?.itemListElement?.at(-1)?.name).toBe("Kubernetes DNS");
});
