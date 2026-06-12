import { useState } from 'react';
import { Link } from 'react-router-dom';
import { KeyRound, Pencil, Plus } from 'lucide-react';
import { api } from '../api/client';
import type { User } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { dateTime, money } from '../lib/format';
import styles from './Users.module.css';

export function UsersPage() {
  const users = useFetch(() => api.users(), []);
  /** id of the user whose Revoke button is awaiting inline confirmation. */
  const [confirmingRevoke, setConfirmingRevoke] = useState<string | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const toast = useToast();

  const onRevoke = async (user: User) => {
    setRevokeBusy(true);
    try {
      await api.revokeUser(user.id);
      users.refetch();
      toast.success(`Revoked "${user.name}".`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Revoke failed.');
    } finally {
      setConfirmingRevoke(null);
      setRevokeBusy(false);
    }
  };

  return (
    <Page
      title="Users"
      actions={
        <Link to="/users/new" className="btn btn-primary">
          <Plus size={15} /> New user
        </Link>
      }
    >
      {users.error ? (
        <ErrorBanner message={users.error} onRetry={users.refetch} />
      ) : users.initialLoading ? (
        <div className={`card ${styles.panel}`} style={{ padding: 16 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : !users.data || users.data.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="No users yet"
          hint="Create a user to let an SDK authenticate against the gateway."
        />
      ) : (
        <div className={`card ${styles.panel}`}>
          <div className={styles.tableScroll}>
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Key prefix</th>
                  <th>Spent / Budget</th>
                  <th>Scope</th>
                  <th className="num">RPM</th>
                  <th>Created</th>
                  <th>Status</th>
                  <th style={{ textAlign: 'right' }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {users.data.map((u) => (
                  <tr key={u.id}>
                    <td>
                      <span className={styles.nameCell}>
                        {u.name}
                        <CaptureBadge capture={u.capture} />
                      </span>
                    </td>
                    <td className="mono">{u.key_prefix}</td>
                    <td>
                      <UsageCell spent={u.spent} budget={u.budget} />
                    </td>
                    <td>
                      <ScopeChips scope={u.scope} />
                    </td>
                    <td className="num">{u.rpm > 0 ? u.rpm : '—'}</td>
                    <td className="mono" style={{ color: 'var(--text-muted)' }}>
                      {dateTime(u.created_at)}
                    </td>
                    <td>
                      <span className={u.active ? styles.statusActive : styles.statusRevoked}>
                        {u.active ? 'Active' : 'Revoked'}
                      </span>
                    </td>
                    <td>
                      <div className={styles.rowActions}>
                        {u.active &&
                          (confirmingRevoke === u.id ? (
                            <>
                              <button
                                className="btn btn-sm btn-danger"
                                disabled={revokeBusy}
                                title="Any SDK using this key stops working immediately. This cannot be undone."
                                onClick={() => onRevoke(u)}
                              >
                                {revokeBusy ? 'Revoking…' : 'Confirm revoke'}
                              </button>
                              <button
                                className="btn btn-sm"
                                disabled={revokeBusy}
                                onClick={() => setConfirmingRevoke(null)}
                              >
                                Cancel
                              </button>
                            </>
                          ) : (
                            <>
                              <Link to={`/users/${u.id}/edit`} className="btn btn-sm">
                                <Pencil size={12} /> Edit
                              </Link>
                              <button
                                className="btn btn-sm btn-danger"
                                onClick={() => setConfirmingRevoke(u.id)}
                              >
                                Revoke
                              </button>
                            </>
                          ))}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </Page>
  );
}

function UsageCell({ spent, budget }: { spent: number; budget: number | null }) {
  if (budget == null || budget <= 0) {
    return (
      <div className={styles.usageCell}>
        <div className={styles.usageTop}>
          <span>{money(spent)}</span>
          <span className="muted">unlimited</span>
        </div>
      </div>
    );
  }
  const ratio = spent / budget;
  const pct = Math.min(100, ratio * 100);
  const fillClass =
    ratio >= 1
      ? styles.usageFillDanger
      : ratio >= 0.8
        ? styles.usageFillAmber
        : '';
  return (
    <div className={styles.usageCell}>
      <div className={styles.usageTop}>
        <span>{money(spent)}</span>
        <span className="muted">{money(budget)}</span>
      </div>
      <div className={styles.usageBar}>
        <div className={`${styles.usageFill} ${fillClass}`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

function CaptureBadge({ capture }: { capture: boolean | null }) {
  if (capture == null) return null;
  return (
    <span className={`chip ${capture ? styles.captureOn : styles.captureOff}`}>
      capture: {capture ? 'on' : 'off'}
    </span>
  );
}

function ScopeChips({ scope }: { scope: string[] }) {
  if (!scope || scope.length === 0) {
    return <span className="muted">all models</span>;
  }
  const shown = scope.slice(0, 3);
  return (
    <div className={styles.scopeChips}>
      {shown.map((s) => (
        <span key={s} className="chip chip-mono">
          {s}
        </span>
      ))}
      {scope.length > shown.length && (
        <span className="chip">+{scope.length - shown.length}</span>
      )}
    </div>
  );
}
