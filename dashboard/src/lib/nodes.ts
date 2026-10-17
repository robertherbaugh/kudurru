// Node readiness as the UI shows it (GET /nodes). Only this node reports
// `ready`; peers leave it out until scatter-gather over the internal
// listener lands, so a missing value is "unknown", not "not ready".
import type { NodeInfo } from '../api/types';

export interface Readiness {
  variant: 'success' | 'error' | 'neutral';
  label: string;
}

export function readiness(n: Pick<NodeInfo, 'ready'>): Readiness {
  if (n.ready === undefined) return { variant: 'neutral', label: 'readiness unknown' };
  return n.ready ? { variant: 'success', label: 'ready' } : { variant: 'error', label: 'not ready' };
}
