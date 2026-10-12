// Deterministic pseudo-random numbers for fixtures (mulberry32), so tests
// and screenshots see the same data on every run.
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function pick<T>(r: () => number, items: readonly T[]): T {
  return items[Math.floor(r() * items.length)] as T;
}

/** Picks with a heavy head (cubic), like real DNS traffic. */
export function pickSkewed<T>(r: () => number, items: readonly T[]): T {
  const x = r();
  return items[Math.floor(x * x * x * items.length)] as T;
}
