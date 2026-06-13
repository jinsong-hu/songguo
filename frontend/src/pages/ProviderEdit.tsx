import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Layers } from 'lucide-react';
import { api } from '../api/client';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { ProviderForm } from '../components/ProviderForm';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';

export function ProviderEditPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const toast = useToast();
  const providers = useFetch(() => api.providers(), []);

  const provider = providers.data?.find((p) => p.id === id);

  return (
    <Page
      title={provider ? `Edit ${provider.name}` : 'Edit provider'}
      actions={
        <Link to="/providers" className="btn">
          <ArrowLeft size={15} /> Back
        </Link>
      }
    >
      {providers.error ? (
        <ErrorBanner message={providers.error} onRetry={providers.refetch} />
      ) : providers.initialLoading ? (
        <div className="card" style={{ maxWidth: 760, padding: 20 }}>
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : !provider ? (
        <EmptyState
          icon={Layers}
          title="Provider not found"
          hint="It may have been removed. Go back to the providers page."
        />
      ) : (
        <ProviderForm
          editing={provider}
          onCancel={() => navigate('/providers')}
          onSaved={() => {
            toast.success('Provider updated.');
            navigate('/providers');
          }}
        />
      )}
    </Page>
  );
}
