// A Policy schedule (spec.schedule) as typed controls: "always" or a
// window, weekday toggles, start and end as time inputs and the time zone
// from the browser's list of IANA zones. The window is [start, end) on the
// chosen weekdays and crosses midnight when end is before start, exactly
// as internal/policy evaluates it; the summary line says whether it would
// be on right now.
import { useId, useMemo } from 'react';
import { scheduleActive, scheduleError, scheduleSummary, WEEKDAYS, weekdayNumber, type Schedule } from '../../lib/policies';

export interface ScheduleFieldProps {
  value: Schedule | undefined;
  onChange: (v: Schedule | undefined) => void;
  isDisabled?: boolean;
  /** Overrides the computed validation message. */
  error?: string;
  /** The clock for the "on right now" line (tests). */
  now?: Date;
}

/** IANA zones of this browser, UTC first. */
export function timeZones(): string[] {
  let zones: string[];
  try {
    zones = Intl.supportedValuesOf('timeZone');
  } catch {
    zones = [];
  }
  return ['UTC', ...zones.filter((z) => z !== 'UTC')];
}

function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

/** The schedule a fresh window starts with: school nights in the browser's zone. */
export function defaultSchedule(): Schedule {
  return { weekdays: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu'], start: '21:00', end: '07:00', tz: browserZone() };
}

export function ScheduleField({ value, onChange, isDisabled, error, now }: ScheduleFieldProps) {
  const id = useId();
  const zones = useMemo(timeZones, []);
  const on = value !== undefined;
  const days = new Set((value?.weekdays ?? []).map((d) => weekdayNumber(d)));
  const message = error ?? (value ? scheduleError(value) : null);
  const set = (patch: Partial<Schedule>) => onChange({ ...(value ?? defaultSchedule()), ...patch });
  const toggleDay = (d: (typeof WEEKDAYS)[number]) => {
    const n = weekdayNumber(d);
    const current = (value?.weekdays ?? []).filter((x) => weekdayNumber(x) !== undefined);
    const next = days.has(n) ? current.filter((x) => weekdayNumber(x) !== n) : [...current, d];
    // Keep the API's Mon..Sun order.
    set({ weekdays: WEEKDAYS.filter((w) => next.some((x) => weekdayNumber(x) === weekdayNumber(w))) });
  };
  const tz = value?.tz || 'UTC';
  return (
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend>When</legend>
      <div className="k-row" role="radiogroup" aria-label="When the policy applies">
        <label className="k-radio">
          <input type="radio" name={`${id}-mode`} checked={!on} onChange={() => onChange(undefined)} /> Always
        </label>
        <label className="k-radio">
          <input type="radio" name={`${id}-mode`} checked={on} onChange={() => onChange(value ?? defaultSchedule())} /> Only during a time window
        </label>
      </div>
      {on ? (
        <>
          <div className="k-stack-sm">
            <span className="k-label">Weekdays</span>
            <div className="k-row" role="group" aria-label="Weekdays">
              {WEEKDAYS.map((d) => (
                <button key={d} type="button" className="k-day" aria-pressed={days.has(weekdayNumber(d))} onClick={() => toggleDay(d)} disabled={isDisabled}>
                  {d}
                </button>
              ))}
            </div>
            <span className="k-muted k-small">None selected means every day. A window that ends after midnight belongs to the day it starts on.</span>
          </div>
          <div className="k-form-grid">
            <label className="k-label">
              Start
              <input className="k-native-input" type="time" value={value?.start ?? ''} onChange={(e) => set({ start: e.target.value })} required />
            </label>
            <label className="k-label">
              End
              <input className="k-native-input" type="time" value={value?.end ?? ''} onChange={(e) => set({ end: e.target.value })} required />
            </label>
            <label className="k-label">
              Time zone
              <select className="k-native-select" value={tz} onChange={(e) => set({ tz: e.target.value })}>
                {zones.includes(tz) ? null : <option value={tz}>{tz}</option>}
                {zones.map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {message ? (
            <span className="k-error-text" role="alert">
              {message}
            </span>
          ) : (
            <span className="k-muted k-small" data-testid="schedule-summary">
              {scheduleSummary(value)} · {scheduleActive(value, now ?? new Date()) ? 'on right now' : 'off right now'}
            </span>
          )}
        </>
      ) : (
        <span className="k-muted k-small">The policy applies at all times.</span>
      )}
    </fieldset>
  );
}
