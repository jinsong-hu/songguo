import { describe, expect, it } from 'vitest';
import type { PressureStats, Status } from '../api/types';
import { bitRate, byteRate, levelRuns, signalName, signalRows } from './status';

const GiB = 1024 ** 3;

function degrade(over: Partial<PressureStats> = {}): PressureStats {
  return {
    level: 'normal',
    since_ms: 0,
    mem_available_bytes: 4 * GiB,
    mem_total_bytes: 8 * GiB,
    disk_free_bytes: 12 * GiB,
    disk_total_bytes: 110 * GiB,
    io_pressure_pct: 2,
    write_lag_ms: 0,
    capture_backlog_bytes: 0,
    capture_budget_bytes: 64 * 1024 ** 2,
    shed_captures: 0,
    shed_analyses: 0,
    thresholds: {
      mem_capture_pct: 30,
      mem_analysis_pct: 10,
      disk_free_min_bytes: 10 * GiB,
      io_pressure_pct: 10,
      write_lag_ms: 1000,
    },
    cooldown_ms: 300_000,
    ...over,
  };
}

function rows(d: PressureStats, host: Status['host'] = { cpus: 4 }) {
  return Object.fromEntries(signalRows({ degrade: d, host }).map((r) => [r.key, r]));
}

describe('signalRows', () => {
  it('measures each reading against its own line', () => {
    // The monitor's reading (5) is what shedding compares; the sampler's host
    // reading (3, a few seconds older) only says PSI is readable.
    const r = rows(degrade({ io_pressure_pct: 5 }), { cpus: 4, io_pressure_pct: 3 });
    expect(r.io_pressure.reading).toBe('5.0%');
    expect(r.io_pressure.fill).toBeCloseTo(0.5);
    expect(r.io_pressure.line).toBe('≥ 10%');
    expect(r.io_pressure.tone).toBe('ok');
    // 50% of memory used against 70% of room above the 30% line.
    expect(r.memory.fill).toBeCloseTo(50 / 70);
    expect(r.memory.tone).toBe('warn');
    // 98 GiB used of the 100 GiB above the 10 GiB line: nearly at it.
    expect(r.disk.fill).toBeCloseTo(0.98);
    expect(r.disk.tone).toBe('warn');
    expect(r.capture_backlog.line).toBe('≥ 32 MB');
  });

  it('marks what the monitor says is over, even before the meter fills', () => {
    const r = rows(degrade({ over: ['write_lag'], write_lag_ms: 400 }));
    expect(r.write_lag.over).toBe(true);
    expect(r.write_lag.tone).toBe('err');
  });

  it('says "—" for a reading that was not taken, and "off" for a disabled line', () => {
    const d = degrade({ mem_available_bytes: undefined, mem_total_bytes: undefined });
    d.thresholds.io_pressure_pct = 0;
    const r = rows(d);
    expect(r.memory.reading).toBe('—');
    expect(r.memory.fill).toBeNull();
    // No host reading and a disabled signal: the monitor's 0 is not a reading.
    expect(r.io_pressure.reading).toBe('—');
    expect(r.io_pressure.line).toBe('off');
  });

  it('has nothing to show without a monitor', () => {
    expect(signalRows({ host: { cpus: 1 } })).toEqual([]);
  });
});

describe('rates', () => {
  it('formats bits and bytes per second', () => {
    expect(bitRate(12_400_000)).toBe('12.4 Mb/s');
    expect(bitRate(840)).toBe('840 b/s');
    expect(bitRate(undefined)).toBe('—');
    expect(byteRate(4.1 * 1024 * 1024)).toBe('4.1 MB/s');
  });
});

describe('levelRuns', () => {
  it('collapses the level series into contiguous runs', () => {
    expect(levelRuns([1, 2, 3, 4, 5], [0, 0, 1, 1, 0])).toEqual([
      { start: 1, end: 2, level: 0 },
      { start: 3, end: 4, level: 1 },
      { start: 5, end: 5, level: 0 },
    ]);
  });
});

describe('signalName', () => {
  it('names known signals and passes unknown ones through', () => {
    expect(signalName('io_pressure')).toBe('host I/O pressure');
    expect(signalName('something_new')).toBe('something_new');
  });
});
