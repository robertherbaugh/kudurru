// List health (§10 principle 6): OK / unchanged / unavailable (serving the
// cached copy) / unavailable / invalid, derived from BlockList status.
import type { BlockList } from '../api/types';

export type HealthState = 'ok' | 'unchanged' | 'cached' | 'unavailable' | 'invalid' | 'pending';

export interface Health {
  state: HealthState;
  label: string;
  detail: string;
  tone: 'success' | 'info' | 'warning' | 'error' | 'neutral';
}

export function listHealth(l: BlockList): Health {
  const st = l.status;
  if (!st || (st.observedGeneration === undefined && st.entries === undefined && !st.conditions?.length)) {
    return { state: 'pending', label: 'pending', detail: 'Not loaded yet', tone: 'neutral' };
  }
  const ready = st.conditions?.find((c) => c.type === 'Ready');
  const failing = (st.sources ?? []).filter((s) => s.error);
  if (ready?.status === 'False') {
    return { state: 'invalid', label: 'invalid', detail: ready.message ?? ready.reason ?? 'Not ready', tone: 'error' };
  }
  if (failing.length) {
    const msg = failing.map((s) => `${s.name}: ${s.error}`).join('; ');
    return (st.entries ?? 0) > 0
      ? { state: 'cached', label: 'unavailable, cached', detail: msg, tone: 'warning' }
      : { state: 'unavailable', label: 'unavailable', detail: msg, tone: 'error' };
  }
  if (ready?.reason === 'Unchanged' || ready?.reason === 'NotModified') {
    return { state: 'unchanged', label: 'unchanged', detail: ready.message ?? 'Source unchanged since the last fetch', tone: 'info' };
  }
  if ((st.invalid ?? 0) > 0) {
    return { state: 'ok', label: `OK, ${st.invalid} invalid`, detail: `${st.invalid} lines could not be parsed and were skipped`, tone: 'warning' };
  }
  return { state: 'ok', label: 'OK', detail: ready?.message ?? 'Loaded', tone: 'success' };
}
