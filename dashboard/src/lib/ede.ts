// Extended DNS Error info codes (RFC 8914 §4 and the IANA registry), for
// the "why" row of the query log.
const EDE: Record<number, string> = {
  0: 'Other error',
  1: 'Unsupported DNSKEY algorithm',
  2: 'Unsupported DS digest type',
  3: 'Stale answer',
  4: 'Forged answer',
  5: 'DNSSEC indeterminate',
  6: 'DNSSEC bogus',
  7: 'Signature expired',
  8: 'Signature not yet valid',
  9: 'DNSKEY missing',
  10: 'RRSIGs missing',
  11: 'No zone key bit set',
  12: 'NSEC missing',
  13: 'Cached error',
  14: 'Not ready',
  15: 'Blocked',
  16: 'Censored',
  17: 'Filtered',
  18: 'Prohibited',
  19: 'Stale NXDOMAIN answer',
  20: 'Not authoritative',
  21: 'Not supported',
  22: 'No reachable authority',
  23: 'Network error',
  24: 'Invalid data',
  25: 'Signature expired before valid',
  26: 'Too early',
  27: 'Unsupported NSEC3 iterations value',
  28: 'Unable to conform to policy',
  29: 'Synthesized',
  30: 'Invalid query type',
};

/** "15 Blocked" or "TYPE 49152" for unknown codes. */
export function edeText(code: number | undefined): string {
  if (code === undefined || code === null) return '–';
  return `${code} ${EDE[code] ?? '(unassigned)'}`;
}
