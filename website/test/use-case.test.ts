import { expect, test } from "bun:test";
import { useCase } from "../src/pages/use-case.ts";
import { USE_CASES } from "../src/use-cases.ts";

test("a renamed use-case page keeps its extra migration link", () => {
  const source = USE_CASES.find((page) => page.path === "/pi-hole-alternative/");
  expect(source).toBeDefined();
  const rendered = useCase({ ...source!, path: "/renamed-alternative/" }).value;
  expect(rendered).toContain('href="https://docs.kudurru.io/migrate/adguard-home/"');
  expect(rendered).toContain("Coming from AdGuard Home");
});
