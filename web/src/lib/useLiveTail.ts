// Live tail of the query log over SSE (GET /queries/stream, §10
// principle 5). EventSource reconnects by itself and re-sends the last
// event id in the Last-Event-ID header; when the user pauses and resumes,
// the last seen id goes into ?last_event_id= on the new connection so the
// server replays what was missed (EventSource cannot set headers).
import { useEffect, useRef, useState } from 'react';
import type { QueryRow, ScopeInfo } from '../api/types';

export type TailState = 'off' | 'connecting' | 'live' | 'retrying' | 'unsupported';

export interface LiveTailOptions {
  /** Stream URL without last_event_id (filters and scope already applied). */
  url: string;
  enabled: boolean;
  /** Called with each batch of new rows, oldest first. */
  onRows: (rows: QueryRow[]) => void;
  onScope?: (scope: ScopeInfo) => void;
}

/** Opens the stream while enabled; returns its state. */
export function useLiveTail({ url, enabled, onRows, onScope }: LiveTailOptions): TailState {
  const [state, setState] = useState<TailState>('off');
  const lastId = useRef<string | undefined>(undefined);
  const handler = useRef(onRows);
  handler.current = onRows;
  const scopeHandler = useRef(onScope);
  scopeHandler.current = onScope;
  const previousURL = useRef(url);

  useEffect(() => {
    if (previousURL.current !== url) { lastId.current = undefined; previousURL.current = url; }
    if (!enabled) {
      setState('off');
      return undefined;
    }
    if (typeof window.EventSource !== 'function') {
      setState('unsupported');
      return undefined;
    }
    const full = lastId.current ? `${url}${url.includes('?') ? '&' : '?'}last_event_id=${encodeURIComponent(lastId.current)}` : url;
    const es = new EventSource(full, { withCredentials: true });
    setState('connecting');
    // Batch rows per animation frame: a busy resolver sends hundreds/s.
    let pending: QueryRow[] = [];
    let frame = 0;
    const flush = () => {
      frame = 0;
      const batch = pending;
      pending = [];
      if (batch.length) handler.current(batch);
    };
    const onQuery = (ev: MessageEvent<string>) => {
      try {
        const row = JSON.parse(ev.data) as QueryRow;
        if (ev.lastEventId) lastId.current = ev.lastEventId;
        pending.push(row);
        if (!frame) frame = window.requestAnimationFrame(flush);
      } catch {
        // ignore malformed events
      }
    };
    es.addEventListener('scope', ((ev: MessageEvent<string>) => {
      try { scopeHandler.current?.(JSON.parse(ev.data) as ScopeInfo); } catch { /* ignore malformed events */ }
    }) as EventListener);
    es.addEventListener('query', onQuery as EventListener);
    es.onmessage = onQuery;
    es.onopen = () => setState('live');
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? 'off' : 'retrying');
    return () => {
      es.close();
      if (frame) window.cancelAnimationFrame(frame);
      flush();
    };
  }, [url, enabled]);

  return state;
}
