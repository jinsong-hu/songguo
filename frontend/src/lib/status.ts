// Pure logic behind the Settings page's live status: what the degrade level
// means, and each shedding signal measured against its own line. Kept apart from
// the components so the arithmetic — including the inverted signals, where LESS
// is worse — is tested without rendering.

import type { PressureSignal, PressureStats, Status } from '../api/types';
import { bytes } from './format';

export type Tone = 'ok' | 'warn' | 'err';

export interface LevelCopy {
  label: string;
  tone: Tone;
  /** What is switched off right now, and what is not. */
  effect: string;
}

export function levelCopy(level: PressureStats['level']): LevelCopy {
  switch (level) {
    case 'shed_capture':
      return {
        label: 'Shedding capture',
        tone: 'warn',
        effect:
          'New calls are not getting captured payloads. Forwarding, metering and call rows are unaffected.',
      };
    case 'shed_analysis':
      return {
        label: 'Shedding capture and analysis',
        tone: 'err',
        effect:
          'Captured payloads, context composition and tool-turn estimates are off. Forwarding, metering and call rows are unaffected.',
      };
    default:
      return {
        label: 'Normal',
        tone: 'ok',
        effect: 'Forwarding with capture and analysis on.',
      };
  }
}

const SIGNAL_NAMES: Record<PressureSignal, string> = {
  io_pressure: 'host I/O pressure',
  memory: 'low memory',
  disk: 'low disk space',
  write_lag: 'slow ledger writes',
  capture_backlog: 'capture backlog',
};

/** Human name of a shedding signal, as a cause ("caused by host I/O pressure"). */
export function signalName(signal: string | undefined): string {
  return (signal && SIGNAL_NAMES[signal as PressureSignal]) || signal || 'unknown';
}

export interface SignalRow {
  key: PressureSignal;
  label: string;
  /** The reading, or "—" when it could not be taken. */
  reading: string;
  /** Where shedding starts, or "off" when the signal is disabled. */
  line: string;
  /** How far toward its line the reading is: 0 = nowhere near, 1 = at the line.
   *  Null when there is no reading or no line to measure against. */
  fill: number | null;
  over: boolean;
  tone: Tone;
  hint: string;
}

/** A meter turns amber this close to its line, before anything is shed. */
export const NEAR_LINE = 0.7;

function toneFor(fill: number | null, over: boolean): Tone {
  if (over || (fill !== null && fill >= 1)) return 'err';
  if (fill !== null && fill >= NEAR_LINE) return 'warn';
  return 'ok';
}

function ratio(value: number, line: number): number | null {
  return line > 0 && Number.isFinite(value) ? Math.max(0, value / line) : null;
}

/**
 * The five shedding signals, each against its threshold. Memory and disk shed
 * when what is LEFT falls below a line, so their fill is the used share of the
 * room above that line — a full bar still means "at the line", in both
 * directions.
 */
export function signalRows(status: Pick<Status, 'degrade' | 'host'>): SignalRow[] {
  const d = status.degrade;
  if (!d) return [];
  const th = d.thresholds;
  const over = new Set(d.over ?? []);
  const rows: Omit<SignalRow, 'tone'>[] = [];

  // I/O pressure: the monitor's own reading is the one compared with the line,
  // but it is 0 rather than absent when PSI cannot be read or the signal is
  // disabled — so the sampler's host reading decides whether there is one, and
  // stands in for it when the signal is off.
  const hostIO = status.host.io_pressure_pct;
  const io = hostIO === undefined ? undefined : th.io_pressure_pct > 0 ? d.io_pressure_pct : hostIO;
  rows.push({
    key: 'io_pressure',
    label: 'Host I/O pressure',
    reading: io === undefined ? '—' : `${io.toFixed(1)}%`,
    line: th.io_pressure_pct > 0 ? `≥ ${th.io_pressure_pct}%` : 'off',
    fill: io === undefined ? null : ratio(io, th.io_pressure_pct),
    over: over.has('io_pressure'),
    hint: 'Share of the last 10 seconds in which some task on the host waited on disk (PSI). Disk is this box’s usual bottleneck.',
  });

  const memAvail = d.mem_available_bytes;
  const memTotal = d.mem_total_bytes;
  const haveMem = memAvail !== undefined && memTotal !== undefined && memTotal > 0;
  const availPct = haveMem ? (memAvail / memTotal) * 100 : undefined;
  rows.push({
    key: 'memory',
    label: 'Memory available',
    reading: haveMem ? `${bytes(memAvail)} (${availPct!.toFixed(0)}%)` : '—',
    line:
      th.mem_capture_pct > 0
        ? `< ${th.mem_capture_pct}%${th.mem_analysis_pct > 0 ? ` · analysis < ${th.mem_analysis_pct}%` : ''}`
        : th.mem_analysis_pct > 0
          ? `analysis < ${th.mem_analysis_pct}%`
          : 'off',
    fill:
      haveMem && th.mem_capture_pct > 0 && th.mem_capture_pct < 100
        ? Math.max(0, (100 - availPct!) / (100 - th.mem_capture_pct))
        : null,
    over: over.has('memory'),
    hint: 'MemAvailable (reclaimable cache counts as free), against the tighter of the container limit and the host. There is no swap.',
  });

  const diskFree = d.disk_free_bytes;
  const diskTotal = d.disk_total_bytes;
  const haveDisk = diskFree !== undefined && diskTotal !== undefined && diskTotal > 0;
  const room = haveDisk ? diskTotal - th.disk_free_min_bytes : 0;
  rows.push({
    key: 'disk',
    label: 'Disk free',
    reading: haveDisk ? `${bytes(diskFree)} of ${bytes(diskTotal)}` : '—',
    line: th.disk_free_min_bytes > 0 ? `< ${bytes(th.disk_free_min_bytes)}` : 'off',
    fill:
      haveDisk && th.disk_free_min_bytes > 0 && room > 0
        ? Math.max(0, (diskTotal - diskFree) / room)
        : null,
    over: over.has('disk'),
    hint: 'Free space on the database’s filesystem. SQLite never gives freed pages back, so this does not recover on its own.',
  });

  rows.push({
    key: 'write_lag',
    label: 'Ledger write lag',
    reading: `${d.write_lag_ms.toLocaleString('en-US')} ms`,
    line: th.write_lag_ms > 0 ? `≥ ${th.write_lag_ms.toLocaleString('en-US')} ms` : 'off',
    fill: ratio(d.write_lag_ms, th.write_lag_ms),
    over: over.has('write_lag'),
    hint: 'The slowest call-record write in the last second, from queue to disk.',
  });

  const backlogLine = d.capture_budget_bytes / 2;
  rows.push({
    key: 'capture_backlog',
    label: 'Capture backlog',
    reading: bytes(d.capture_backlog_bytes),
    line: backlogLine > 0 ? `≥ ${bytes(backlogLine)}` : 'off',
    fill: ratio(d.capture_backlog_bytes, backlogLine),
    over: over.has('capture_backlog'),
    hint: 'Captured bodies queued for the database but not yet written. Sheds at half the capture budget.',
  });

  return rows.map((r) => ({ ...r, tone: toneFor(r.fill, r.over) }));
}

/** Bits per second, SI: "840 b/s", "12.4 Mb/s". */
export function bitRate(bps: number | undefined | null): string {
  if (bps === undefined || bps === null || !Number.isFinite(bps)) return '—';
  const units = ['b/s', 'Kb/s', 'Mb/s', 'Gb/s'];
  let v = bps;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i += 1;
  }
  return `${i === 0 || v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** Bytes per second: "0 B/s", "4.1 MB/s". */
export function byteRate(bps: number | undefined | null): string {
  if (bps === undefined || bps === null || !Number.isFinite(bps)) return '—';
  return `${bytes(Math.round(bps))}/s`;
}

/** A percentage reading, or "—" when absent. */
export function pct(n: number | undefined | null, digits = 1): string {
  return n === undefined || n === null || !Number.isFinite(n) ? '—' : `${n.toFixed(digits)}%`;
}

/** Contiguous runs of one degrade level across the history, for the timeline strip. */
export function levelRuns(t: number[], level: number[]): { start: number; end: number; level: number }[] {
  const runs: { start: number; end: number; level: number }[] = [];
  for (let i = 0; i < t.length; i += 1) {
    const last = runs[runs.length - 1];
    if (last && last.level === level[i]) {
      last.end = t[i];
    } else {
      runs.push({ start: t[i], end: t[i], level: level[i] ?? 0 });
    }
  }
  return runs;
}
