import { expect, test } from "bun:test";
import { readTheme, saveTheme, themeColorMedia } from "../src/client/theme-preference.ts";

test("only supported overrides are read from storage and System removes the key", () => {
  const values = new Map<string, string>();
  const storage = { setItem: values.set.bind(values), removeItem: values.delete.bind(values) };
  for (const invalid of [null, undefined, "system", "", "DARK", "auto"]) expect(readTheme(invalid)).toBe("system");
  for (const expected of ["light", "dark"] as const) {
    saveTheme(storage, expected);
    expect(readTheme(values.get("kudurru-theme"))).toBe(expected);
  }
  saveTheme(storage, "system");
  expect(values.has("kudurru-theme")).toBe(false);
});

test("theme-color follows OS in System and matches exactly one forced scheme", () => {
  for (const scheme of ["light", "dark"] as const) {
    expect(themeColorMedia("system", scheme)).toBe(`(prefers-color-scheme: ${scheme})`);
    expect(themeColorMedia(scheme, scheme)).toBe("all");
    expect(themeColorMedia(scheme === "dark" ? "light" : "dark", scheme)).toBe("not all");
  }
});
