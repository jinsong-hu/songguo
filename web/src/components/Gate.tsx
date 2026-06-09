import { useState, type FormEvent } from 'react';
import { setAdminKey } from '../api/client';
import styles from './Gate.module.css';

interface GateProps {
  /** Called after the key is verified; receives the validated key. */
  onAuthenticated: () => void;
  /** Verifies a candidate key by calling the API; throws on failure. */
  verify: () => Promise<void>;
}

export function Gate({ onAuthenticated, verify }: GateProps) {
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!key.trim() || busy) return;
    setBusy(true);
    setError(null);
    setAdminKey(key.trim());
    try {
      await verify();
      onAuthenticated();
    } catch {
      setError('Invalid admin key. Check the key and try again.');
      setBusy(false);
    }
  };

  return (
    <div className={styles.wrap}>
      <form className={`card ${styles.card}`} onSubmit={submit}>
        <img className={styles.logo} src="/songguo-mark.svg" alt="Songguo" />
        <div className={styles.title}>Songguo</div>
        <div className={styles.subtitle}>Sign in to the dashboard</div>

        <div className={styles.form}>
          <label className={styles.label} htmlFor="admin-key">
            Admin key
          </label>
          <input
            id="admin-key"
            className="input"
            type="password"
            autoFocus
            autoComplete="current-password"
            placeholder="••••••••••••"
            value={key}
            onChange={(e) => setKey(e.target.value)}
          />
          {error && <div className={styles.error}>{error}</div>}
          <button
            type="submit"
            className={`btn btn-primary ${styles.submit}`}
            disabled={busy || !key.trim()}
          >
            {busy ? 'Verifying…' : 'Continue'}
          </button>
        </div>
      </form>
    </div>
  );
}
