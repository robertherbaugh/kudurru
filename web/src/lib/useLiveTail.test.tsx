import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { useLiveTail } from './useLiveTail';

class FakeEventSource extends EventTarget {
  static CLOSED = 2;
  static instances: FakeEventSource[] = [];
  readyState = 1;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(readonly url: string) { super(); FakeEventSource.instances.push(this); }
  close() { this.readyState = FakeEventSource.CLOSED; }
  emit(type: string, data: unknown, lastEventId = '') { this.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(data), lastEventId })); }
}

afterEach(() => { vi.unstubAllGlobals(); FakeEventSource.instances = []; });

describe('useLiveTail', () => {
  it('resumes with opaque cluster IDs on pause and resets them on a scope or filter change', () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    const url = '/api/v1/queries/stream?scope=cluster';
    const onRows = vi.fn();
    const { rerender } = renderHook(({ url, enabled }) => useLiveTail({ url, enabled, onRows }), { initialProps: { url, enabled: true } });
    act(() => FakeEventSource.instances[0]!.emit('query', mockDB().queries[0], 'opaque-cluster-id'));
    rerender({ url, enabled: false });
    rerender({ url, enabled: true });
    expect(FakeEventSource.instances[1]!.url).toBe(`${url}&last_event_id=opaque-cluster-id`);
    const nodeURL = '/api/v1/queries/stream?scope=node&node=node-b';
    rerender({ url: nodeURL, enabled: true });
    expect(FakeEventSource.instances[2]!.url).toBe(nodeURL);
  });

  it('reports partial scope updates from a live stream', () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    const onScope = vi.fn();
    renderHook(() => useLiveTail({ url: '/stream', enabled: true, onRows: vi.fn(), onScope }));
    const scope = { scope: 'cluster', nodes: [{ name: 'node-a', ok: true }, { name: 'node-b', ok: false, error: 'live tail: unavailable' }], partial: true };
    act(() => FakeEventSource.instances[0]!.emit('scope', scope));
    expect(onScope).toHaveBeenCalledWith(scope);
  });
});
