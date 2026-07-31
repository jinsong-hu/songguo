import { useEffect, useMemo, useRef, useState, type CSSProperties } from 'react';
import { Link, useLocation, useParams } from 'react-router-dom';
import { ArrowLeft, Layers } from 'lucide-react';
import { api } from '../api/client';
import type { Catalog, Provider, Service } from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Playground } from '../components/Playground';
import {
  RoutingConfigCard,
  routingSignature,
  type RoutingConfigItem,
} from '../components/RoutingConfigCard';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { useSession } from '../lib/sessionContext';
import { contextLabel, indexCatalog, MODALITY_LABEL, type CatalogInfo } from '../lib/catalogIndex';
import { BrandIcon, ModelIcon, modelMeta, providerBrand } from '../lib/modelBrand';
import styles from './ServiceDetail.module.css';

export function ServiceDetailPage() {
  const { model = '' } = useParams();
  const { data, error, initialLoading, refetch } = useFetch(() => api.services(), []);
  const { data: catalog } = useFetch(() => api.catalog(), []);
  const { data: providers } = useFetch(() => api.providers(), []);
  const { me } = useSession();

  const service = data?.find((s) => s.model === model);
  const info = indexCatalog(catalog).get(model);
  const meta = modelMeta(model);

  return (
    <Page
      title={meta.name}
      actions={
        <Link to="/services" className="btn">
          <ArrowLeft size={15} /> All services
        </Link>
      }
    >
      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading ? (
        <div className={styles.stack}>
          <Skeleton height={120} />
          <Skeleton height={80} />
          <Skeleton height={160} />
        </div>
      ) : !service ? (
        <EmptyState
          icon={Layers}
          title="Model not found"
          hint={
            <>
              No provider currently serves <code>{model}</code>.{' '}
              <Link to="/services">Back to services</Link>.
            </>
          }
        />
      ) : (
        <div className={styles.stack}>
          <Hero model={model} info={info} />
          {me.role === 'admin' && (
            <ServiceRouting
              model={model}
              service={service}
              providers={providers ?? []}
              onSaved={refetch}
            />
          )}
          <TestSection
            services={data ?? []}
            providers={providers ?? []}
            catalog={catalog}
            model={model}
          />
        </div>
      )}
    </Page>
  );
}

function ServiceRouting({
  model,
  service,
  providers,
  onSaved,
}: {
  model: string;
  service: Service;
  providers: Provider[];
  onSaved: () => void;
}) {
  const toast = useToast();
  const initial = useMemo(
    () =>
      service.providers.map((route): RoutingConfigItem => {
        const provider = providers.find((item) => item.id === route.id);
        const brand = providerBrand(
          provider?.vendor ?? route.name,
          provider?.models.map((item) => item.model) ?? [model],
        );
        const complete =
          !!provider &&
          provider.masked_key !== '' &&
          provider.endpoints.length > 0 &&
          provider.models.length > 0;
        return {
          id: route.id,
          name: route.name,
          icon: <BrandIcon brand={brand} label={route.name} size={17} />,
          href: `/providers/${route.id}/edit`,
          color: brand?.color ?? '#3f8f5b',
          enabled: route.enabled,
          available: route.provider_enabled && complete,
          custom: route.priority_override !== null || route.weight_override !== null,
          priority: String(route.priority),
          weight: String(route.weight),
          defaultPriority: route.default_priority,
          defaultWeight: route.default_weight,
          unavailableLabel: !route.provider_enabled ? 'Provider off' : 'Incomplete',
        };
      }),
    [model, providers, service.providers],
  );
  const [items, setItems] = useState(initial);
  const [saving, setSaving] = useState(false);

  useEffect(() => setItems(initial), [initial]);

  const dirty = routingSignature(items) !== routingSignature(initial);
  const change = (id: string, patch: Partial<RoutingConfigItem>) => {
    setItems((current) => current.map((item) => (item.id === id ? { ...item, ...patch } : item)));
  };

  const save = async () => {
    for (const item of items) {
      const priority = Number(item.priority);
      const weight = Number(item.weight);
      if (item.custom && (!Number.isInteger(priority) || priority < 0)) {
        toast.error(`${item.name}: priority must be a whole number of 0 or greater.`);
        return;
      }
      // Blank is caught before the range check: Number('') is 0, and 0 now parks
      // the provider for this model — a cleared field must not silently mean that.
      if (item.custom && (item.weight.trim() === '' || !Number.isInteger(weight) || weight < 0)) {
        toast.error(
          `${item.name}: weight must be a whole number of 0 or greater. 0 parks it — no new sessions.`,
        );
        return;
      }
    }

    setSaving(true);
    try {
      for (const item of items) {
        await api.patchServiceProviderRouting(item.id, {
          model,
          enabled: item.enabled,
          ...(item.custom
            ? { priority: Number(item.priority), weight: Number(item.weight) }
            : { inherit_priority: true, inherit_weight: true }),
        });
      }
      toast.success('Service routing updated.');
      onSaved();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : 'Could not update service routing.');
    } finally {
      setSaving(false);
    }
  };

  return (
    <RoutingConfigCard
      title="Routing"
      hint="Lower priority numbers are strict failover tiers. Weight controls the share of new sessions within one priority; existing sessions stay pinned while their provider is healthy. Weight 0 parks a provider for this model — no share of its tier, but still reachable by an explicit provider pin; a custom weight here also gives a provider parked by default a share of this one service."
      items={items}
      saving={saving}
      dirty={dirty}
      editableEnabled
      inherited
      onChange={change}
      onSave={save}
    />
  );
}

/** The playground card, scrolled into view when the URL is /services/:model#test. */
function TestSection({
  services,
  providers,
  catalog,
  model,
}: {
  services: Service[];
  providers: Provider[];
  catalog: Catalog | null;
  model: string;
}) {
  const { hash } = useLocation();
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (hash === '#test') ref.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [hash]);

  return (
    <div id="test" ref={ref}>
      <Playground services={services} providers={providers} catalog={catalog} defaultModel={model} />
    </div>
  );
}

function Hero({ model, info }: { model: string; info?: CatalogInfo }) {
  const meta = modelMeta(model);
  const context = contextLabel(info?.context);
  const modalities = (info?.modalities ?? []).map((m) => MODALITY_LABEL[m] ?? m);

  const facts: Array<[string, string]> = [];
  if (context) facts.push(['Context window', `${context} tokens`]);
  if (modalities.length > 0) facts.push(['Modalities', modalities.join(' · ')]);
  if (info && info.input > 0) facts.push(['Input', `$${info.input} / 1M tokens`]);
  if (info && info.output > 0) facts.push(['Output', `$${info.output} / 1M tokens`]);
  if (info?.cached_input) facts.push(['Cached input', `$${info.cached_input} / 1M tokens`]);

  return (
    <div className={`card ${styles.hero}`} style={{ '--brand': meta.color } as CSSProperties}>
      <div className={styles.heroMain}>
        <span className={styles.iconTile}>
          <ModelIcon model={model} size={30} />
        </span>
        <div className={styles.heroText}>
          <h2 className={styles.heroName}>{meta.name}</h2>
          <p className={styles.heroTagline}>{meta.tagline}</p>
          <div className={styles.heroId}>
            <code>{model}</code>
            <CopyButton value={model} />
          </div>
        </div>
      </div>
      {facts.length > 0 && (
        <div className={styles.facts}>
          {facts.map(([label, value]) => (
            <div key={label} className={styles.fact}>
              <span className={styles.factLabel}>{label}</span>
              <span className={styles.factValue}>{value}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
