import { useMemo } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { ArrowLeft, Layers } from 'lucide-react';
import { api } from '../api/client';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { ProviderForm, type ProviderPrefill } from '../components/ProviderForm';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';

export function ProviderNewPage() {
  const [params] = useSearchParams();
  const presetId = params.get('preset');
  const navigate = useNavigate();
  const toast = useToast();
  // The catalog is only needed to seed the form from a preset.
  const catalog = useFetch(() => api.catalog(), [], { enabled: !!presetId });

  const preset = useMemo<{ title: string; prefill: ProviderPrefill } | null>(() => {
    if (!presetId) return null;
    for (const vendor of catalog.data?.vendors ?? []) {
      const service = vendor.services.find((s) => s.id === presetId);
      if (!service) continue;
      return {
        title: `Add ${vendor.name} ${service.name}`,
        prefill: {
          name: service.id,
          vendor: vendor.name,
          adapter: service.adapter,
          base_url: service.base_url,
          catalog_id: service.id,
          wires: service.wires,
          quirks: service.quirks,
          models: service.models.map((m) => ({
            model: m.model,
            input: m.input,
            output: m.output,
            cached_input: m.cached_input ?? 0,
            unit: m.unit,
          })),
        },
      };
    }
    return null;
  }, [presetId, catalog.data]);

  const onSaved = () => {
    toast.success('Provider added.');
    navigate('/services/add');
  };

  return (
    <Page
      title={preset?.title ?? 'Add provider'}
      actions={
        <Link to="/services/add" className="btn">
          <ArrowLeft size={15} /> Back
        </Link>
      }
    >
      {presetId && catalog.error ? (
        <ErrorBanner message={catalog.error} onRetry={catalog.refetch} />
      ) : presetId && catalog.initialLoading ? (
        <div className="card" style={{ maxWidth: 760, padding: 20 }}>
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      ) : presetId && !preset ? (
        <EmptyState
          icon={Layers}
          title="Preset not found"
          hint="The catalog preset no longer exists. Configure a custom provider instead."
        />
      ) : (
        <ProviderForm
          prefill={preset?.prefill}
          onCancel={() => navigate('/services/add')}
          onSaved={onSaved}
        />
      )}
    </Page>
  );
}
