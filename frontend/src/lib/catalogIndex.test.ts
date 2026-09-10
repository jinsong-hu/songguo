import { describe, expect, it } from 'vitest';

import { peakLabel } from './catalogIndex';
import type { Cost, CostWindow } from '../api/types';

function win(days: string[], start: string, end: string, offset = '+08:00'): CostWindow {
  return { type: 'weekly', offset, days, start, end };
}

const WEEKDAYS = ['mon', 'tue', 'wed', 'thu', 'fri'];

describe('peakLabel', () => {
  it('is empty for the models that have no schedule, which is nearly all of them', () => {
    expect(peakLabel({ input: 1, output: 4 })).toBe('');
    expect(peakLabel({})).toBe('');
  });

  it('renders DeepSeek exactly as the vendor states it', () => {
    const cost: Cost = {
      input: 0.15,
      output: 0.6,
      schedules: [
        {
          input: 0.3,
          output: 1.2,
          when: [win(WEEKDAYS, '09:00', '12:00'), win(WEEKDAYS, '14:00', '18:00')],
        },
      ],
    };
    // The multiple is what an operator is deciding on; the hours stay in the
    // vendor's own zone so the label can be checked against their pricing page.
    expect(peakLabel(cost)).toBe('2x · Mon–Fri 09:00–12:00, 14:00–18:00 UTC+08:00');
  });

  it('collapses a contiguous day range and keeps a gappy one as a list', () => {
    const range: Cost = {
      input: 1,
      schedules: [{ input: 2, when: [win(['mon', 'tue', 'wed'], '09:00', '10:00')] }],
    };
    expect(peakLabel(range)).toBe('2x · Mon–Wed 09:00–10:00 UTC+08:00');

    const gappy: Cost = {
      input: 1,
      schedules: [{ input: 2, when: [win(['mon', 'wed', 'fri'], '09:00', '10:00')] }],
    };
    expect(peakLabel(gappy)).toBe('2x · Mon, Wed, Fri 09:00–10:00 UTC+08:00');
  });

  it('names a single day without a range dash', () => {
    const cost: Cost = {
      input: 1,
      schedules: [{ input: 3, when: [win(['sun'], '00:00', '06:00')] }],
    };
    expect(peakLabel(cost)).toBe('3x · Sun 00:00–06:00 UTC+08:00');
  });

  it('says UTC rather than "UTC" plus an offset when there is none', () => {
    const cost: Cost = {
      input: 1,
      schedules: [{ input: 2, when: [{ type: 'weekly', days: ['mon'], start: '09:00', end: '12:00' }] }],
    };
    expect(peakLabel(cost)).toBe('2x · Mon 09:00–12:00 UTC');
  });

  it('spells the days out per window when they differ', () => {
    const cost: Cost = {
      input: 1,
      schedules: [
        { input: 2, when: [win(['mon'], '09:00', '12:00'), win(['sat'], '14:00', '18:00')] },
      ],
    };
    expect(peakLabel(cost)).toBe('2x · Mon 09:00–12:00 UTC+08:00, Sat 14:00–18:00 UTC+08:00');
  });

  it('takes the multiple off the dominant token side, like tierLabel', () => {
    // Output is the larger axis, and the schedule raises it 4x while input only
    // doubles. The label must report the worse number, not the first one.
    const cost: Cost = {
      input: 1,
      output: 5,
      schedules: [{ input: 2, output: 20, when: [win(['mon'], '09:00', '12:00')] }],
    };
    expect(peakLabel(cost)).toBe('4x · Mon 09:00–12:00 UTC+08:00');
  });

  it('ignores a window type it does not understand rather than half-rendering it', () => {
    const cost: Cost = {
      input: 1,
      schedules: [
        { input: 2, when: [{ type: 'lunar', days: ['mon'], start: '09:00', end: '12:00' }] },
      ],
    };
    expect(peakLabel(cost)).toBe('');
  });

  it('still names a schedule whose multiple cannot be computed', () => {
    // No base rate to divide by — say the hours are raised rather than print
    // "NaNx" or silently drop the fact that a peak exists.
    const cost: Cost = {
      schedules: [{ input: 2, when: [win(['mon'], '09:00', '12:00')] }],
    };
    expect(peakLabel(cost)).toBe('raised Mon 09:00–12:00 UTC+08:00');
  });
});
