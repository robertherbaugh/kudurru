// A Policy schedule (spec.schedule) as typed controls: "always" or a
// window (RadioList), weekday toggles, start and end as TimeInputs and the time zone
// from the browser's list of IANA zones. The window is [start, end) on the
// chosen weekdays and crosses midnight when end is before start, exactly
// as internal/policy evaluates it; the summary line says whether it would
// be on right now.
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { RadioList, RadioListItem } from '@astryxdesign/core/RadioList';
import { Text } from '@astryxdesign/core/Text';
import { TimeInput, type ISOTimeString } from '@astryxdesign/core/TimeInput';
import { ToggleButton } from '@astryxdesign/core/ToggleButton';
import { VStack } from '@astryxdesign/core/VStack';
import { useId, useMemo } from 'react';
import { SelectField } from './SelectField';
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
  const time = (v: string | undefined) => (v ? (v as ISOTimeString) : undefined);
  return (
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend>When</legend>
      <RadioList label="When the policy applies" isLabelHidden orientation="horizontal" value={on ? 'window' : 'always'} isDisabled={isDisabled} onChange={(v) => (v === 'always' ? onChange(undefined) : onChange(value ?? defaultSchedule()))}>
        <RadioListItem value="always" label="Always" />
        <RadioListItem value="window" label="Only during a time window" />
      </RadioList>
      {on ? (
        <>
          <VStack gap={1}>
            <Text type="label" id={`${id}-days`}>
              Weekdays
            </Text>
            <HStack gap={1} wrap="wrap" role="group" aria-labelledby={`${id}-days`} aria-describedby={`${id}-days-help`}>
              {WEEKDAYS.map((d) => (
                <ToggleButton key={d} label={d} size="sm" isPressed={days.has(weekdayNumber(d))} onPressedChange={() => toggleDay(d)} isDisabled={isDisabled} />
              ))}
            </HStack>
            <Text type="supporting" display="block" id={`${id}-days-help`}>
              None selected means every day. A window that ends after midnight belongs to the day it starts on.
            </Text>
          </VStack>
          <Grid columns={{ minWidth: 180, repeat: 'fit' }} gap={3} align="start">
            <TimeInput label="Start" value={time(value?.start)} onChange={(v) => set({ start: v ?? '' })} hourFormat="24h" isRequired isDisabled={isDisabled} />
            <TimeInput label="End" value={time(value?.end)} onChange={(v) => set({ end: v ?? '' })} hourFormat="24h" isRequired isDisabled={isDisabled} />
            <SelectField label="Time zone" value={tz} options={zones.map((z) => ({ value: z, label: z }))} onChange={(z) => set({ tz: z })} hasSearch isDisabled={isDisabled} />
          </Grid>
          {message ? (
            <FieldStatus type="error" message={message} variant="detached" />
          ) : (
            <Text type="supporting" display="block" data-testid="schedule-summary">
              {scheduleSummary(value)} · {scheduleActive(value, now ?? new Date()) ? 'on right now' : 'off right now'}
            </Text>
          )}
        </>
      ) : (
        <Text type="supporting" display="block">
          The policy applies at all times.
        </Text>
      )}
    </fieldset>
  );
}
