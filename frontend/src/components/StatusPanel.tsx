import type { ReactNode } from 'react';
import { api } from '../api/client';
import type { Status } from '../api/types';
import { bytes, duration, int } from '../lib/format';
import {
  bitRate,
  byteRate,
  levelCopy,
  levelRuns,
  pct,
  signalName,
  signalRows,
  type SignalRow,
} from '../lib/status';
import { useFetch, useLiveTick } from '../lib/useFetch';
import { ErrorBanner } from './ErrorBanner';
import { InfoHint } from './InfoHint';
import { Skeleton } from './Skeleton';
import { Sparkline } from './Sparkline';
import styles from './StatusPanel.module.css';

/** Matches the backend sampler's interval; a faster poll would only repeat it. */
const POLL_MS = 5_000;

const clockFmt = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

/**
 * Live status for the Settings page: is songguo degrading and why, how loaded is
 * the host, and how loaded is songguo itself. Three cards, one poll of
 * GET /api/status — which reads memory and /proc only, so leaving this page open
 * on a struggling gateway costs it nothing.
 */
export function StatusPanel() {
  const res = useFetch(() => api.status(), [], { intervalMs: POLL_MS });
  const now = useLiveTick(1_000) * 1_000;

  if (res.initialLoading) {
    return (
      <div className={`card ${styles.panel}`}>
        <div className={styles.title}>Status</div>
        <div className={styles.skeletons}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={24} />
          ))}
        </div>
      </div>
    );
  }
  if (!res.data) {
    return (
      <div className={`card ${styles.panel}`}>
        <div className={styles.title}>Status</div>
        <ErrorBanner message={res.error ?? 'Status is unavailable.'} onRetry={res.refetch} />
      </div>
    );
  }

  const s = res.data;
  return (
    <>
      <DegradeCard status={s} now={now} error={res.error} />
      <HostCard status={s} />
      <GatewayCard status={s} now={now} />
    </>
  );
}

function DegradeCard({ status: s, now, error }: { status: Status; now: number; error: string | null }) {
  const d = s.degrade;
  const age = Math.max(0, now - s.sampled_at_ms);
  const stale = error !== null || age > s.interval_ms * 3;
  const copy = levelCopy(d?.level ?? 'normal');

  return (
    <div className={`card ${styles.panel}`}>
      <div className={styles.head}>
        <div>
          <div className={styles.title}>Status</div>
          <div className={styles.desc}>
            Live from memory and /proc every {s.interval_ms / 1000}s — never the database.
          </div>
        </div>
        <span className={stale ? `${styles.fresh} ${styles.freshStale}` : styles.fresh}>
          <span className={styles.freshDot} />
          {error ? `Not updating: ${error}` : `Sampled ${duration(age / 1000)} ago`}
        </span>
      </div>

      {!d ? (
        <div className={styles.effect}>The degrade monitor is not running in this process.</div>
      ) : (
        <>
          <div className={styles.levelRow}>
            <span className={`pill pill-${copy.tone} ${styles.levelPill}`}>
              <span className="dot" />
              {copy.label}
            </span>
            <span className={styles.levelSince}>
              since {clockFmt.format(new Date(d.since_ms))} ({duration((now - d.since_ms) / 1000)})
            </span>
          </div>
          <div className={styles.effect}>{copy.effect}</div>
          {d.level !== 'normal' && (
            <div className={styles.cause}>
              Caused by <strong>{signalName(d.reason)}</strong>.{' '}
              {d.restore_at_ms
                ? `Readings are clear; restores in ${duration(Math.max(0, d.restore_at_ms - now) / 1000)} if they stay that way.`
                : d.over && d.over.length > 0
                  ? `Still over the line: ${d.over.map(signalName).join(', ')}.`
                  : ''}
            </div>
          )}

          <Timeline status={s} />

          <div className={styles.tableScroll}>
            <table className={`table ${styles.signals}`}>
              <thead>
                <tr>
                  <th>Shedding signal</th>
                  <th className={styles.numHead}>Reading</th>
                  <th className={styles.numHead}>Sheds at</th>
                  <th className={styles.meterCol}>
                    <span className={styles.srOnly}>Toward the line</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {signalRows(s).map((r) => (
                  <SignalTableRow key={r.key} row={r} />
                ))}
              </tbody>
            </table>
          </div>

          <div className={styles.counters}>
            Since songguo started {duration((now - s.started_at_ms) / 1000)} ago:{' '}
            <Count n={d.shed_captures} label="calls not captured" />
            {' · '}
            <Count n={d.shed_analyses} label="analyses skipped" />
            {s.gateway.ledger && (
              <>
                {' · '}
                <Count n={s.gateway.ledger.payloads_shed} label="payloads dropped over the capture budget" />
              </>
            )}
          </div>
        </>
      )}
    </div>
  );
}

function Count({ n, label }: { n: number; label: string }) {
  return (
    <span className={n > 0 ? styles.countHot : undefined}>
      {int(n)} {label}
    </span>
  );
}

function SignalTableRow({ row: r }: { row: SignalRow }) {
  const width = r.fill === null ? 0 : Math.min(1, r.fill) * 100;
  return (
    <tr className={r.over ? styles.overRow : undefined}>
      <td>
        <span className={styles.signalLabel}>
          {r.label}
          <InfoHint text={r.hint} label={r.label} />
        </span>
      </td>
      <td className="num">{r.reading}</td>
      <td className={`num ${styles.muted}`}>{r.line}</td>
      <td className={styles.meterCol}>
        {r.fill === null ? (
          <span className={styles.muted}>—</span>
        ) : (
          <span
            className={`${styles.meter} ${styles[`meter_${r.tone}`]}`}
            role="meter"
            aria-label={`${r.label}: ${Math.round(r.fill * 100)}% of the way to its shedding line`}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(Math.min(1, r.fill) * 100)}
            title={`${Math.round(r.fill * 100)}% of the way to the line`}
          >
            <span className={styles.meterFill} style={{ width: `${width}%` }} />
          </span>
        )}
      </td>
    </tr>
  );
}

/** The degrade level across the history window, one segment per run. */
function Timeline({ status: s }: { status: Status }) {
  const { t, level } = s.history;
  if (t.length < 2) return null;
  const span = t[t.length - 1] - t[0] || 1;
  const runs = levelRuns(t, level);
  const names = ['Normal', 'Shedding capture', 'Shedding capture and analysis'];
  return (
    <div className={styles.timeline}>
      <div className={styles.timelineBar} role="img" aria-label="Degrade level over the recent window">
        {runs.map((r, i) => {
          // Each run extends to the next one's start, so segments tile the bar.
          const end = i + 1 < runs.length ? runs[i + 1].start : r.end;
          const grow = Math.max(end - r.start, s.interval_ms / 2);
          return (
            <span
              key={r.start}
              className={`${styles.seg} ${styles[`seg${r.level}`]}`}
              style={{ flexGrow: grow }}
              title={`${clockFmt.format(new Date(r.start))}–${clockFmt.format(new Date(end))} · ${names[r.level] ?? 'Unknown'}`}
            />
          );
        })}
      </div>
      <div className={styles.timelineAxis}>
        <span>{duration(span / 1000)} ago</span>
        <span>now</span>
      </div>
    </div>
  );
}

function HostCard({ status: s }: { status: Status }) {
  const h = s.host;
  const hist = s.history;
  const load =
    h.load1 === undefined
      ? '—'
      : `${h.load1.toFixed(2)} · ${h.load5?.toFixed(2)} · ${h.load15?.toFixed(2)}`;
  return (
    <div className={`card ${styles.panel}`}>
      <div className={styles.title}>Host load</div>
      <div className={styles.desc}>
        The whole machine — songguo shares it with everything else on the box.
      </div>
      <div className={styles.tiles}>
        <Tile
          label="CPU"
          value={pct(h.cpu_pct, 0)}
          sub={`load ${load} on ${h.cpus} CPU${h.cpus === 1 ? '' : 's'}`}
          hint="Busy share of all CPUs over the last sample. Load averages count runnable and I/O-blocked tasks over 1, 5 and 15 minutes."
        >
          <Sparkline t={hist.t} values={hist.cpu_pct} format={(v) => pct(v, 0)} label="Host CPU" max={100} />
        </Tile>
        <Tile
          label="I/O pressure"
          value={pct(h.io_pressure_pct)}
          sub={`1-min average ${pct(h.io_pressure_avg60_pct)}`}
          hint="Share of time some task waited on disk (PSI). The dashed line is where capture is shed."
        >
          <Sparkline
            t={hist.t}
            values={hist.io_pressure_pct}
            format={(v) => pct(v)}
            label="Host I/O pressure"
            line={s.degrade?.thresholds.io_pressure_pct || undefined}
          />
        </Tile>
        <Tile
          label="CPU & memory stalls"
          value={`${pct(h.cpu_pressure_pct)} · ${pct(h.memory_pressure_pct)}`}
          sub="PSI: time tasks waited for CPU · for memory"
          hint="Non-zero memory stall means the kernel is reclaiming under pressure; on a box without swap that comes before the OOM killer."
        />
      </div>
    </div>
  );
}

function GatewayCard({ status: s, now }: { status: Status; now: number }) {
  const g = s.gateway;
  const p = s.process;
  const hist = s.history;
  const inFlight = hist.in_flight.map((v) => v as number | null);
  return (
    <div className={`card ${styles.panel}`}>
      <div className={styles.title}>Songguo load</div>
      <div className={styles.desc}>What this process is carrying right now.</div>
      <div className={styles.tiles}>
        <Tile
          label="In flight"
          value={int(g.in_flight)}
          sub={`${int(g.waiting)} waiting for a provider slot · ${g.requests_per_min === undefined ? '—' : g.requests_per_min.toFixed(1)} req/min`}
          hint="Requests being handled now. A response still streaming and an open WebSocket count until they end. Waiting requests are held by a provider's max concurrency."
        >
          <Sparkline t={hist.t} values={inFlight} format={(v) => int(v)} label="In-flight requests" />
        </Tile>
        <Tile
          label="Network out"
          value={bitRate(s.network.tx_bps)}
          sub={`in ${bitRate(s.network.rx_bps)}`}
          hint="Songguo's own interfaces (its container's network). Out is mostly request bodies re-sent to providers, so an agent resending a long context shows up here first. Compare with the server's bandwidth cap."
        >
          <Sparkline t={hist.t} values={hist.net_tx_bps} format={bitRate} label="Network out" />
        </Tile>
        <Tile
          label="Network in"
          value={bitRate(s.network.rx_bps)}
          sub="clients' requests and providers' responses"
        >
          <Sparkline t={hist.t} values={hist.net_rx_bps} format={bitRate} label="Network in" />
        </Tile>
        <Tile
          label="Process CPU"
          value={pct(p.cpu_pct, 0)}
          sub={`one core = 100% · ${int(p.goroutines)} goroutines`}
        >
          <Sparkline t={hist.t} values={hist.process_cpu_pct} format={(v) => pct(v, 0)} label="Process CPU" />
        </Tile>
        <Tile
          label="Disk writes"
          value={byteRate(p.disk_write_bps)}
          sub={`reads ${byteRate(p.disk_read_bps)}`}
          hint="Bytes this process wrote and read at the storage layer — the disk load songguo itself adds. Page-cache hits are not counted."
        >
          <Sparkline t={hist.t} values={hist.disk_write_bps} format={byteRate} label="Disk writes" />
        </Tile>
        <Tile
          label="Memory"
          value={p.rss_bytes === undefined ? '—' : bytes(p.rss_bytes)}
          sub={`resident · ${bytes(g.buffered_request_bytes)} of request bodies buffered`}
          hint="Resident memory of the songguo process. Request bodies are buffered whole while a call is in flight, so this grows with body size × concurrency."
        />
      </div>

      <div className={styles.meta}>
        {g.ledger && (
          <>
            <span className={styles.metaKey}>Ledger queue</span>
            <span>
              {int(g.ledger.depth)} / {int(g.ledger.capacity)} queued · peak {int(g.ledger.high_water)} ·{' '}
              {int(g.ledger.written)} written
              {g.ledger.failed > 0 && <span className={styles.countHot}> · {int(g.ledger.failed)} failed</span>}
              {' · '}
              {g.ledger.blocked === 0 ? (
                'no request has waited on it'
              ) : (
                <span className={styles.countHot}>
                  {int(g.ledger.blocked)} requests waited, {int(g.ledger.blocked_ms)} ms total
                </span>
              )}
            </span>
          </>
        )}
        <span className={styles.metaKey}>Database</span>
        <span>
          {s.database.size_bytes === undefined ? '—' : bytes(s.database.size_bytes)} · WAL{' '}
          {s.database.wal_bytes === undefined ? '—' : bytes(s.database.wal_bytes)}
        </span>
        {s.drain && (
          <>
            <span className={styles.metaKey}>Background cleanup</span>
            <DrainLine drain={s.drain} now={now} />
          </>
        )}
      </div>
    </div>
  );
}

const DRAIN_STATE: Record<NonNullable<Status['drain']>['state'], string> = {
  pending: 'starting',
  running: 'running',
  paused: 'paused while the disk is busy',
  retrying: 'retrying after an error',
  done: 'done',
};

function DrainLine({ drain, now }: { drain: NonNullable<Status['drain']>; now: number }) {
  const started = drain.started_at_ms ? ` · started ${duration((now - drain.started_at_ms) / 1000)} ago` : '';
  return (
    <span>
      Retiring <code>parsed_calls</code> a few rows at a time — {DRAIN_STATE[drain.state]} · {int(drain.rows)} rows
      {drain.state !== 'done' && drain.rows > 0 && ` · last batch ${int(drain.last_batch_ms)} ms`}
      {started}
    </span>
  );
}

function Tile({
  label,
  value,
  sub,
  hint,
  children,
}: {
  label: string;
  value: string;
  sub?: string;
  hint?: string;
  children?: ReactNode;
}) {
  return (
    <div className={styles.tile}>
      <div className={styles.tileLabel}>
        {label}
        {hint && <InfoHint text={hint} label={label} />}
      </div>
      <div className={styles.tileValue}>{value}</div>
      {sub && <div className={styles.tileSub}>{sub}</div>}
      {children}
    </div>
  );
}

