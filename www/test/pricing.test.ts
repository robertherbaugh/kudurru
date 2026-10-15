import { describe, expect, test } from "bun:test";
import { annualPrice, usd } from "../src/pricing.ts";

describe("annual commercial pricing", () => {
  test.each([
    [1, 198, 798], [3, 396, 1396], [5, 594, 1994], [10, 1089, 3489],
    [24, 2475, 7675], [25, 2326.5, 7226.5], [49, 4464.9, 13684.9],
  ])("quotes %i nodes for both editions", (nodes, core, enterprise) => {
    expect(annualPrice("core", nodes!)?.total).toBe(core);
    expect(annualPrice("enterprise", nodes!)?.total).toBe(enterprise);
  });

  test("discounts node fees only, leaving the platform fee unchanged", () => {
    expect(annualPrice("enterprise", 25)).toEqual({
      platform: 499, perNode: 269.1, nodeFee: 6727.5, total: 7226.5, discounted: true,
    });
    expect(annualPrice("core", 24)?.discounted).toBe(false);
  });

  test("requires individual quotes from 50 nodes and rejects invalid counts", () => {
    for (const nodes of [50, 51, 1000, 0, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      expect(annualPrice("core", nodes)).toBeNull();
      expect(annualPrice("enterprise", nodes)).toBeNull();
    }
  });

  test("formats annual totals and monthly equivalents in USD", () => {
    expect(usd(1994)).toBe("$1,994");
    expect(usd(7226.5)).toBe("$7,226.50");
    expect(usd(1994 / 12, true)).toBe("$166.17");
  });
});
