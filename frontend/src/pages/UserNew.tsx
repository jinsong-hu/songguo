import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { AlertTriangle, ArrowLeft } from 'lucide-react';
import { api } from '../api/client';
import type { User } from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { UserForm } from '../components/UserForm';
import { useFetch } from '../lib/useFetch';
import styles from './UserNew.module.css';

export function UserNewPage() {
  const navigate = useNavigate();
  const pricing = useFetch(() => api.pricing(), []);
  const [created, setCreated] = useState<User | null>(null);

  const modelOptions = useMemo(() => {
    const set = new Set<string>();
    for (const row of pricing.data ?? []) set.add(row.model);
    return Array.from(set).sort((a, b) => a.localeCompare(b));
  }, [pricing.data]);

  return (
    <Page
      title={created ? 'User created' : 'New user'}
      actions={
        <Link to="/users" className="btn">
          <ArrowLeft size={15} /> Back to users
        </Link>
      }
    >
      {created ? (
        <div className={`card ${styles.revealCard}`}>
          <div className={styles.warnBox}>
            <AlertTriangle size={16} style={{ flexShrink: 0, marginTop: 1 }} />
            <span>
              Copy this key now — it won&apos;t be shown again. Store it somewhere safe.
            </span>
          </div>
          <div>
            <div className={styles.keyLabel}>{created.name}</div>
            <div className={styles.keyField}>
              <code className={styles.keyValue}>{created.key}</code>
              <CopyButton value={created.key ?? ''} label="Copy" />
            </div>
          </div>
          <div className={styles.footerRow}>
            <button className="btn btn-primary" onClick={() => navigate('/users')}>
              Done
            </button>
          </div>
        </div>
      ) : pricing.error ? (
        <ErrorBanner message={pricing.error} onRetry={pricing.refetch} />
      ) : pricing.initialLoading ? (
        <div className="card" style={{ maxWidth: 560, padding: 20 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : (
        <UserForm
          modelOptions={modelOptions}
          onCancel={() => navigate('/users')}
          onCreated={setCreated}
        />
      )}
    </Page>
  );
}
