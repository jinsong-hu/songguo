import { useState } from 'react';
import { Link } from 'react-router-dom';
import { KeyRound, Pencil, Plus } from 'lucide-react';
import { api } from '../api/client';
import type { User } from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { dateTime, money } from '../lib/format';
import styles from './Users.module.css';

/** The seeded admin user mirrors the .env admin key and cannot be deleted. */
const ADMIN_ID = 'admin';

export function UsersPage() {
  const users = useFetch(() => api.users(), []);
  /** id of the user whose Delete button is awaiting inline confirmation. */
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const toast = useToast();

  const onDelete = async (user: User) => {
    setDeleteBusy(true);
    try {
      await api.deleteUser(user.id);
      users.refetch();
      toast.success(`Deleted "${user.name}".`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Delete failed.');
    } finally {
      setConfirmingDelete(null);
      setDeleteBusy(false);
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
                  <th>Key</th>
                  <th>Spent / Budget</th>
                  <th>Last Seen</th>
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
                    <td>
                      <KeyCell user={u} />
                    </td>
                    <td>
                      <UsageCell spent={u.spent} budget={u.budget} />
                    </td>
                    <td className="mono" style={{ color: 'var(--text-muted)' }}>
                      {u.last_seen ? dateTime(u.last_seen) : '—'}
                    </td>
                    <td>
                      <span className={u.active ? styles.statusActive : styles.statusRevoked}>
                        {u.active ? 'Active' : 'Revoked'}
                      </span>
                    </td>
                    <td>
                      <div className={styles.rowActions}>
                        {confirmingDelete === u.id ? (
                          <>
                            <button
                              className="btn btn-sm btn-danger"
                              disabled={deleteBusy}
                              title="This permanently removes the user and its key. This cannot be undone."
                              onClick={() => onDelete(u)}
                            >
                              {deleteBusy ? 'Deleting…' : 'Confirm delete'}
                            </button>
                            <button
                              className="btn btn-sm"
                              disabled={deleteBusy}
                              onClick={() => setConfirmingDelete(null)}
                            >
                              Cancel
                            </button>
                          </>
                        ) : (
                          <>
                            <Link to={`/users/${u.id}/edit`} className="btn btn-sm">
                              <Pencil size={12} /> Edit
                            </Link>
                            {u.id !== ADMIN_ID && (
                              <button
                                className="btn btn-sm btn-danger"
                                onClick={() => setConfirmingDelete(u.id)}
                              >
                                Delete
                              </button>
                            )}
                          </>
                        )}
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

/** Masks a plaintext key for display, e.g. "sg-abc***def". */
function maskKey(key: string): string {
  if (key.length <= 9) return key;
  return `${key.slice(0, 6)}***${key.slice(-3)}`;
}

/** Renders a masked key with a copy button for the full key. */
function KeyCell({ user }: { user: User }) {
  const full = user.key ?? '';
  const display = full ? maskKey(full) : `${user.key_prefix}…`;
  return (
    <div className={styles.keyCell}>
      <span className="mono">{display}</span>
      {full && <CopyButton value={full} className={styles.keyCopy} />}
    </div>
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
