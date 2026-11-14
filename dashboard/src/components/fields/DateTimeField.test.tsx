import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { DateTimeField, utcInput } from './DateTimeField';
import { formatError } from './formats';

describe('absolute settings timestamps', () => {
  it('normalizes an offset expiry to UTC without shifting its instant', () => {
    expect(utcInput('2026-10-15T09:30:12-05:00')).toBe('2026-10-15T14:30:12');
    expect(utcInput('2026-10-15T14:30:12Z')).toBe('2026-10-15T14:30:12');
    expect(utcInput('')).toBeUndefined();
    expect(utcInput('2026-10-15T14:30:12')).toBeUndefined();
    expect(formatError('date-time', '7d')).toBeTruthy();
  });

  it('names the time zone and links the expiry help to the date control', () => {
    render(<DateTimeField label="Expires at" value="2026-10-15T14:30:12Z" onChange={vi.fn()} help="Stops bypassing validation at this instant." />);
    const date = screen.getByLabelText('Expires at (UTC)');
    expect(date).toHaveAccessibleDescription('Stops bypassing validation at this instant.');
    expect(screen.getByLabelText('Expires at (UTC) time')).toBeInTheDocument();
  });
});
