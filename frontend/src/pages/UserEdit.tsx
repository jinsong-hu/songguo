import { useMemo } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, KeyRound } from 'lucide-react';
import { api } from '../api/client';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { UserForm } from '../components/UserForm';
import { useFetch } from '../lib/useFetch';

export function UserEditPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const toast = useToast();
  const users = useFetch(() => api.users(), []);
  const pricing = useFetch(() => api.pricing(), []);

  const modelOptions = useMemo(() => {
    const set = new Set<string>();
    for (const row of pricing.data ?? []) set.add(row.model);
    return Array.from(set).sort((a, b) => a.localeCompare(b));
  }, [pricing.data]);

  const user = users.data?.find((u) => u.id === id);
  const loading = users.initialLoading || pricing.initialLoading;
  const error = users.error ?? pricing.error;

  return (
    <Page
      title={user ? `Edit ${user.name}` : 'Edit user'}
      actions={
        <Link to="/users" className="btn">
          <ArrowLeft size={15} /> Back to users
        </Link>
      }
    >
      {error ? (
        <ErrorBanner
          message={error}
          onRetry={() => {
            users.refetch();
            pricing.refetch();
          }}
        />
      ) : loading ? (
        <div className="card" style={{ maxWidth: 560, padding: 20 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : !user ? (
        <EmptyState
          icon={KeyRound}
          title="User not found"
          hint="It may have been deleted. Go back to the users list."
        />
      ) : (
        <UserForm
          user={user}
          modelOptions={modelOptions}
          onCancel={() => navigate('/users')}
          onSaved={() => {
            toast.success('User updated.');
            navigate('/users');
          }}
        />
      )}
    </Page>
  );
}
