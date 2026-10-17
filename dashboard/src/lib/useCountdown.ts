// A one-second ticking countdown towards an RFC 3339 instant.
import { useEffect, useState } from 'react';

/**
 * Seconds left until `until` (never negative), re-rendering once a second
 * while the countdown runs; undefined when there is no deadline. When the
 * deadline passes, `onExpire` runs once (e.g. to refetch the state).
 */
export function useCountdown(until: string | undefined, onExpire?: () => void): number | undefined {
  const target = until ? new Date(until).getTime() : Number.NaN;
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (Number.isNaN(target)) return undefined;
    setNow(Date.now());
    const id = window.setInterval(() => {
      const t = Date.now();
      setNow(t);
      if (t >= target) window.clearInterval(id);
    }, 1000);
    return () => window.clearInterval(id);
  }, [target]);

  const left = Number.isNaN(target) ? undefined : Math.max(0, Math.ceil((target - now) / 1000));
  const expired = left === 0;
  useEffect(() => {
    if (expired) onExpire?.();
    // onExpire is intentionally not a dependency: fire once per expiry.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expired]);
  return left;
}
