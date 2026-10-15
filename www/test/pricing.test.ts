import { describe, expect, test } from "bun:test";
import { annualPrice, pricingState, usd } from "../src/pricing.ts";

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

describe("calculator presentation", () => {
  test("switches editions with the same node count", () => {
    expect(pricingState("enterprise", 5)).toEqual({
      kind: "price", platform: "$499", nodeLabel: "5 × $299 per node", nodeFee: "$1,495", total: "$1,994",
      message: "Equivalent to $166.17/month, billed annually.",
    });
    expect(pricingState("core", 5)).toEqual({
      kind: "price", platform: "$99", nodeLabel: "5 × $99 per node", nodeFee: "$495", total: "$594",
      message: "Equivalent to $49.50/month, billed annually.",
    });
  });

  test("explains the discounted node fee and unchanged platform fee", () => {
    expect(pricingState("enterprise", 25)).toEqual({
      kind: "price", platform: "$499", nodeLabel: "25 × $269.10 per node (10% off)", nodeFee: "$6,727.50", total: "$7,226.50",
      message: "Equivalent to $602.21/month, billed annually. Includes 10% off node fees; the platform fee is unchanged.",
    });
  });

  test("replaces amounts with an individual quote at 50 or more nodes", () => {
    for (const nodes of [50, 100]) {
      expect(pricingState("enterprise", nodes)).toEqual({
        kind: "quote", total: "Let’s size it together",
        message: "Deployments of 50 or more nodes are quoted individually. Contact us for volume pricing.",
      });
    }
  });

  test("rejects empty, nonnumeric, fractional, nonpositive and unsafe counts", () => {
    // Empty fields and native number-input errors arrive as NaN.
    for (const nodes of [NaN, Infinity, -1, 0, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
      expect(pricingState("core", nodes)).toEqual({
        kind: "invalid", message: "Enter a whole number of production nodes, 1 or more.",
      });
    }
  });

  test("returns to a valid price after invalid and quote states", () => {
    for (const nodes of [NaN, 5, 50, 5]) {
      const state = pricingState("core", nodes);
      if (nodes === 5) {
        expect(state).toMatchObject({ kind: "price", total: "$594", nodeFee: "$495" });
        expect(state.message).toBe("Equivalent to $49.50/month, billed annually.");
      } else {
        expect(state.kind).toBe(Number.isNaN(nodes) ? "invalid" : "quote");
        expect(state).not.toHaveProperty("nodeFee");
      }
    }
  });
});
