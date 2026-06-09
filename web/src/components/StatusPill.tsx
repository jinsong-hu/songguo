import { statusKind } from '../lib/format';

/** A colored status pill: 2xx accent, 4xx amber, 0/5xx danger. */
export function StatusPill({ status }: { status: number }) {
  const kind = statusKind(status);
  const cls = kind === 'ok' ? 'pill-ok' : kind === 'warn' ? 'pill-warn' : 'pill-err';
  const label = status === 0 ? 'ERR' : String(status);
  return (
    <span className={`pill ${cls}`}>
      <span className="dot" />
      {label}
    </span>
  );
}
