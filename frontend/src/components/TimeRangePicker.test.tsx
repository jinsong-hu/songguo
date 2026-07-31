import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { TimeRangePicker } from './TimeRangePicker';
import { DEFAULT_RANGE, type TimeRange } from '../lib/timeRange';

// Vitest runs with globals:false, so Testing Library cannot register its own
// afterEach — without this, each test's DOM leaks into the next and queries
// match elements from a previous render.
afterEach(cleanup);

/** Wrapper that holds state, so the control is exercised the way the page uses it. */
function Harness({ onChange }: { onChange?: (r: TimeRange) => void }) {
  const [range, setRange] = useState<TimeRange>(DEFAULT_RANGE);
  return (
    <TimeRangePicker
      value={range}
      onChange={(r) => {
        setRange(r);
        onChange?.(r);
      }}
    />
  );
}

const openPanel = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(screen.getByRole('button', { name: /Last 24 hours/ }));
  return screen.getByRole('dialog', { name: 'Time range' });
};

describe('TimeRangePicker', () => {
  it('shows the active range on the trigger and opens on click', async () => {
    const user = userEvent.setup();
    render(<Harness />);

    expect(screen.getByRole('button', { name: /Last 24 hours/ })).toBeDefined();
    expect(screen.queryByRole('dialog')).toBeNull();

    await openPanel(user);
    expect(screen.getByRole('dialog', { name: 'Time range' })).toBeDefined();
  });

  it('renders the calendar grid', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const panel = await openPanel(user);

    // react-day-picker renders the month as a real grid with day buttons.
    expect(within(panel).getByRole('grid')).toBeDefined();
    expect(within(panel).getAllByRole('gridcell').length).toBeGreaterThan(27);
  });

  it('applies a preset and closes', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const panel = await openPanel(user);

    await user.click(within(panel).getByRole('button', { name: 'Last 7 days' }));

    expect(onChange).toHaveBeenCalledWith({ kind: 'rolling', from: 'now-7d', to: 'now' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('button', { name: /Last 7 days/ })).toBeDefined();
  });

  it('applies a typed expression', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const panel = await openPanel(user);

    const fromField = within(panel).getByLabelText('From');
    await user.clear(fromField);
    await user.type(fromField, 'now-45m');
    await user.click(within(panel).getByRole('button', { name: 'Apply time range' }));

    expect(onChange).toHaveBeenCalledWith({ kind: 'rolling', from: 'now-45m', to: 'now' });
    expect(screen.getByRole('button', { name: /Last 45 minutes/ })).toBeDefined();
  });

  it('refuses a malformed expression and stays open', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const panel = await openPanel(user);

    const fromField = within(panel).getByLabelText('From');
    await user.clear(fromField);
    await user.type(fromField, 'now-5q');
    await user.click(within(panel).getByRole('button', { name: 'Apply time range' }));

    expect(onChange).not.toHaveBeenCalled();
    expect(within(panel).getByRole('alert')).toBeDefined();
    expect(screen.getByRole('dialog')).toBeDefined();
  });

  it('writes a calendar click back into the fields as an absolute range', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const panel = await openPanel(user);

    // Day buttons are named by their full date ("Wednesday, July 8th, 2026"), so
    // match on the cell's data-day rather than the visible number — a bare digit
    // regex also matches presets like "Last 15 minutes".
    const days = within(panel)
      .getAllByRole('gridcell')
      .map((cell) => ({ cell, btn: cell.querySelector('button') }))
      .filter((d): d is { cell: HTMLElement; btn: HTMLButtonElement } => !!d.btn && !d.btn.disabled);
    expect(days.length).toBeGreaterThan(20);

    const target = days[Math.floor(days.length / 2)];
    const isoDay = target.cell.getAttribute('data-day');
    await user.click(target.btn);

    // A clicked day pins the range: no `now`, snapped to that day's start.
    const fromField = within(panel).getByLabelText('From') as HTMLInputElement;
    expect(fromField.value).toBe(`${isoDay} 00:00`);
    expect(screen.getByRole('dialog')).toBeDefined();
  });

  it('closes on Escape without applying', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await openPanel(user);

    await user.keyboard('{Escape}');

    expect(screen.queryByRole('dialog')).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('discards an abandoned edit when reopened', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    let panel = await openPanel(user);

    await user.clear(within(panel).getByLabelText('From'));
    await user.type(within(panel).getByLabelText('From'), 'now-3h');
    await user.keyboard('{Escape}');

    panel = await openPanel(user);
    expect((within(panel).getByLabelText('From') as HTMLInputElement).value).toBe('now-24h');
  });
});
