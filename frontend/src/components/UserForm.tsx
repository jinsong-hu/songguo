import { useState, type FormEvent } from 'react';
import { api } from '../api/client';
import type { CreateUserBody, PatchUserBody, User } from '../api/types';
import styles from './UserForm.module.css';

/** Three-way capture override choice shown in the user form. */
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

interface UserFormProps {
  /** When set, the form edits this user; otherwise it creates a new one. */
  user?: User;
  modelOptions: string[];
  onCancel: () => void;
  /** Called with the freshly created user (includes the one-time key). */
  onCreated?: (u: User) => void;
  onSaved?: () => void;
}

export function UserForm({ user, modelOptions, onCancel, onCreated, onSaved }: UserFormProps) {
  const editing = !!user;
  const [name, setName] = useState(user?.name ?? '');
  const [budget, setBudget] = useState(
    user?.budget != null ? String(user.budget) : '',
  );
  const [rpm, setRpm] = useState(user?.rpm ? String(user.rpm) : '');
  const [scope, setScope] = useState<string[]>(user?.scope ?? []);
  const [capture, setCapture] = useState<CaptureChoice>(toCaptureChoice(user?.capture));
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
      if (editing && user) {
        const body: PatchUserBody = {
          name: trimmed,
          budget: budgetVal,
          scope,
          rpm: rpmVal,
          capture: captureVal,
        };
        await api.patchUser(user.id, body);
        onSaved?.();
      } else {
        const body: CreateUserBody = {
          name: trimmed,
          budget: budgetVal,
          scope,
          rpm: rpmVal,
          capture: captureVal,
        };
        const created = await api.createUser(body);
        onCreated?.(created);
      }
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <form className={`card ${styles.formCard}`} onSubmit={submit}>
      <div className={styles.field}>
        <label className={styles.fieldLabel} htmlFor="u-name">
          Name
        </label>
        <input
          id="u-name"
          className="input"
          value={name}
          autoFocus
          placeholder="e.g. production-app"
          onChange={(e) => setName(e.target.value)}
        />
      </div>

      <div className={styles.field}>
        <label className={styles.fieldLabel} htmlFor="u-budget">
          Budget (USD)
        </label>
        <input
          id="u-budget"
          className="input"
          inputMode="decimal"
          value={budget}
          placeholder="Leave blank for unlimited"
          onChange={(e) => setBudget(e.target.value)}
        />
        <span className={styles.fieldHint}>Optional spend cap in dollars.</span>
      </div>

      <div className={styles.field}>
        <label className={styles.fieldLabel} htmlFor="u-rpm">
          RPM limit
        </label>
        <input
          id="u-rpm"
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
          Capture request/response payloads for this user. Default inherits the global
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

      {err && <div className={styles.error}>{err}</div>}

      <div className={styles.footerRow}>
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? 'Saving…' : editing ? 'Save changes' : 'Create user'}
        </button>
      </div>
    </form>
  );
}
