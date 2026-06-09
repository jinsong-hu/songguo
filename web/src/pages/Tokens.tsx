import { useMemo, useState, type FormEvent } from 'react';
import { AlertTriangle, KeyRound, Pencil, Plus } from 'lucide-react';
import { api } from '../api/client';
import type { CreateTokenBody, PatchTokenBody, Token } from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Modal } from '../components/Modal';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { dateTime, money } from '../lib/format';
import styles from './Tokens.module.css';

type ModalState =
  | { kind: 'none' }
  | { kind: 'create' }
  | { kind: 'edit'; token: Token }
  | { kind: 'reveal'; token: Token }
  | { kind: 'revoke'; token: Token };

/** Three-way capture override choice shown in the token form. */
type CaptureChoice = 'default' | 'on' | 'off';

const CAPTURE_CHOICES: { value: CaptureChoice; label: string }[] = [
  { value: 'default', label: 'Default' },
  { value: 'on', label: 'On' },
  { value: 'off', label: 'Off' },
];

function toCaptureChoice(capture: boolean | null | undefined): CaptureChoice {
  if (capture === true) return 'on';
  if (capture === false) return 'off';
  return 'default';
}

function fromCaptureChoice(choice: CaptureChoice): boolean | null {
  if (choice === 'on') return true;
  if (choice === 'off') return false;
  return null;
}

export function TokensPage() {
  const tokens = useFetch(() => api.tokens(), []);
  const pricing = useFetch(() => api.pricing(), []);
  const [modal, setModal] = useState<ModalState>({ kind: 'none' });
  const toast = useToast();

  const modelOptions = useMemo(() => {
    const set = new Set<string>();
    for (const row of pricing.data ?? []) set.add(row.model);
    return Array.from(set).sort((a, b) => a.localeCompare(b));
  }, [pricing.data]);

  const closeModal = () => setModal({ kind: 'none' });

  const onCreated = (token: Token) => {
    tokens.refetch();
    setModal({ kind: 'reveal', token });
  };

  const onSaved = () => {
    tokens.refetch();
    closeModal();
    toast.success('Token updated.');
  };

  const onRevoked = async (token: Token) => {
    try {
      await api.revokeToken(token.id);
      tokens.refetch();
      closeModal();
      toast.success(`Revoked “${token.name}”.`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Revoke failed.');
    }
  };

  return (
    <Page
      title="Tokens"
      actions={
        <button className="btn btn-primary" onClick={() => setModal({ kind: 'create' })}>
          <Plus size={15} /> New token
        </button>
      }
    >
      {tokens.error ? (
        <ErrorBanner message={tokens.error} onRetry={tokens.refetch} />
      ) : tokens.initialLoading ? (
        <div className={`card ${styles.panel}`} style={{ padding: 16 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : !tokens.data || tokens.data.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="No tokens yet"
          hint="Create a token to let an SDK authenticate against the gateway."
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
                {tokens.data.map((t) => (
                  <tr key={t.id}>
                    <td>
                      <span className={styles.nameCell}>
                        {t.name}
                        <CaptureBadge capture={t.capture} />
                      </span>
                    </td>
                    <td className="mono">{t.key_prefix}</td>
                    <td>
                      <UsageCell spent={t.spent} budget={t.budget} />
                    </td>
                    <td>
                      <ScopeChips scope={t.scope} />
                    </td>
                    <td className="num">{t.rpm > 0 ? t.rpm : '—'}</td>
                    <td className="mono" style={{ color: 'var(--text-muted)' }}>
                      {dateTime(t.created_at)}
                    </td>
                    <td>
                      <span className={t.active ? styles.statusActive : styles.statusRevoked}>
                        {t.active ? 'Active' : 'Revoked'}
                      </span>
                    </td>
                    <td>
                      <div className={styles.rowActions}>
                        {t.active && (
                          <>
                            <button
                              className="btn btn-sm"
                              onClick={() => setModal({ kind: 'edit', token: t })}
                            >
                              <Pencil size={12} /> Edit
                            </button>
                            <button
                              className="btn btn-sm btn-danger"
                              onClick={() => setModal({ kind: 'revoke', token: t })}
                            >
                              Revoke
                            </button>
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

      {(modal.kind === 'create' || modal.kind === 'edit') && (
        <TokenForm
          token={modal.kind === 'edit' ? modal.token : undefined}
          modelOptions={modelOptions}
          onClose={closeModal}
          onCreated={onCreated}
          onSaved={onSaved}
        />
      )}

      {modal.kind === 'reveal' && (
        <RevealModal token={modal.token} onClose={closeModal} />
      )}

      {modal.kind === 'revoke' && (
        <RevokeModal
          token={modal.token}
          onClose={closeModal}
          onConfirm={() => onRevoked(modal.token)}
        />
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

interface TokenFormProps {
  token?: Token;
  modelOptions: string[];
  onClose: () => void;
  onCreated: (t: Token) => void;
  onSaved: () => void;
}

function TokenForm({ token, modelOptions, onClose, onCreated, onSaved }: TokenFormProps) {
  const editing = !!token;
  const [name, setName] = useState(token?.name ?? '');
  const [budget, setBudget] = useState(
    token?.budget != null ? String(token.budget) : '',
  );
  const [rpm, setRpm] = useState(token?.rpm ? String(token.rpm) : '');
  const [scope, setScope] = useState<string[]>(token?.scope ?? []);
  const [capture, setCapture] = useState<CaptureChoice>(toCaptureChoice(token?.capture));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const toggleScope = (model: string) => {
    setScope((prev) =>
      prev.includes(model) ? prev.filter((m) => m !== model) : [...prev, model],
    );
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const trimmed = name.trim();
    if (!trimmed) {
      setErr('Name is required.');
      return;
    }
    const budgetVal: number | null = budget.trim() === '' ? null : Number(budget);
    if (budgetVal != null && (Number.isNaN(budgetVal) || budgetVal < 0)) {
      setErr('Budget must be a non-negative number.');
      return;
    }
    const rpmVal = rpm.trim() === '' ? 0 : Number(rpm);
    if (Number.isNaN(rpmVal) || rpmVal < 0) {
      setErr('RPM must be a non-negative number.');
      return;
    }
    const captureVal = fromCaptureChoice(capture);

    setBusy(true);
    setErr(null);
    try {
      if (editing && token) {
        const body: PatchTokenBody = {
          name: trimmed,
          budget: budgetVal,
          scope,
          rpm: rpmVal,
          capture: captureVal,
        };
        await api.patchToken(token.id, body);
        onSaved();
      } else {
        const body: CreateTokenBody = {
          name: trimmed,
          budget: budgetVal,
          scope,
          rpm: rpmVal,
          capture: captureVal,
        };
        const created = await api.createToken(body);
        onCreated(created);
      }
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <Modal
      title={editing ? 'Edit token' : 'New token'}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            type="submit"
            form="token-form"
            className="btn btn-primary"
            disabled={busy}
          >
            {busy ? 'Saving…' : editing ? 'Save changes' : 'Create token'}
          </button>
        </>
      }
    >
      <form id="token-form" onSubmit={submit}>
        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor="t-name">
            Name
          </label>
          <input
            id="t-name"
            className="input"
            value={name}
            autoFocus
            placeholder="e.g. production-app"
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor="t-budget">
            Budget (USD)
          </label>
          <input
            id="t-budget"
            className="input"
            inputMode="decimal"
            value={budget}
            placeholder="Leave blank for unlimited"
            onChange={(e) => setBudget(e.target.value)}
          />
          <span className={styles.fieldHint}>Optional spend cap in dollars.</span>
        </div>

        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor="t-rpm">
            RPM limit
          </label>
          <input
            id="t-rpm"
            className="input"
            inputMode="numeric"
            value={rpm}
            placeholder="0 = unlimited"
            onChange={(e) => setRpm(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <span className={styles.fieldLabel}>Scope</span>
          <span className={styles.fieldHint}>
            Restrict to specific models. None selected = all models.
          </span>
          {modelOptions.length === 0 ? (
            <span className="muted" style={{ fontSize: 12.5 }}>
              No priced models available.
            </span>
          ) : (
            <div className={styles.scopeBox}>
              {modelOptions.map((m) => (
                <label key={m} className={styles.scopeOpt}>
                  <input
                    type="checkbox"
                    checked={scope.includes(m)}
                    onChange={() => toggleScope(m)}
                  />
                  {m}
                </label>
              ))}
            </div>
          )}
        </div>

        <div className={styles.field}>
          <span className={styles.fieldLabel}>Capture</span>
          <span className={styles.fieldHint}>
            Capture request/response payloads for this token. Default inherits the global
            setting.
          </span>
          <div className={styles.captureSeg} role="group" aria-label="Capture override">
            {CAPTURE_CHOICES.map((c) => (
              <button
                key={c.value}
                type="button"
                className={`${styles.captureBtn} ${
                  capture === c.value ? styles.captureBtnActive : ''
                }`}
                aria-pressed={capture === c.value}
                onClick={() => setCapture(c.value)}
              >
                {c.label}
              </button>
            ))}
          </div>
        </div>

        {err && (
          <div style={{ color: 'var(--danger)', fontSize: 12.5, marginTop: 4 }}>{err}</div>
        )}
      </form>
    </Modal>
  );
}

function RevealModal({ token, onClose }: { token: Token; onClose: () => void }) {
  return (
    <Modal
      title="Token created"
      onClose={onClose}
      footer={
        <button className="btn btn-primary" onClick={onClose}>
          Done
        </button>
      }
    >
      <div className={styles.reveal}>
        <div className={styles.warnBox}>
          <AlertTriangle size={16} style={{ flexShrink: 0, marginTop: 1 }} />
          <span>
            Copy this key now — it won&apos;t be shown again. Store it somewhere safe.
          </span>
        </div>
        <div>
          <div className={styles.fieldLabel} style={{ marginBottom: 6 }}>
            {token.name}
          </div>
          <div className={styles.keyField}>
            <code className={styles.keyValue}>{token.key}</code>
            <CopyButton value={token.key ?? ''} label="Copy" />
          </div>
        </div>
      </div>
    </Modal>
  );
}

function RevokeModal({
  token,
  onClose,
  onConfirm,
}: {
  token: Token;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <Modal
      title="Revoke token"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            className="btn btn-danger"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              onConfirm();
            }}
          >
            {busy ? 'Revoking…' : 'Revoke token'}
          </button>
        </>
      }
    >
      <p className={styles.confirmText}>
        Revoke <strong>{token.name}</strong> (<strong>{token.key_prefix}</strong>)? Any SDK
        using this key will immediately stop working. This cannot be undone.
      </p>
    </Modal>
  );
}
