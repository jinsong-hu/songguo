import type { CSSProperties } from 'react';
import { Link, useParams } from 'react-router-dom';
import { ArrowLeft, Layers } from 'lucide-react';
import { api } from '../api/client';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { contextLabel, indexCatalog, MODALITY_LABEL, type CatalogInfo } from '../lib/catalogIndex';
import { ModelIcon, modelMeta } from '../lib/modelBrand';
import { int, ms, percent } from '../lib/format';
import styles from './ServiceDetail.module.css';

export function ServiceDetailPage() {
  const { model = '' } = useParams();
  const { data, error, initialLoading, refetch } = useFetch(() => api.services(), []);
  const { data: catalog } = useFetch(() => api.catalog(), []);

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
          <QuickStart model={model} kind={info?.kind ?? 'chat'} />
          <Usage
            requests={service.stats.requests}
            errors={service.stats.errors}
            avgLatency={service.stats.avg_latency_ms}
          />
        </div>
      )}
    </Page>
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

function QuickStart({ model, kind }: { model: string; kind: string }) {
  const origin = window.location.origin;
  const snippet =
    kind === 'embedding'
      ? `curl ${origin}/v1/embeddings \\
  -H "Authorization: Bearer $SONGGUO_TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${model}",
    "input": "The quick brown fox"
  }'`
      : `curl ${origin}/v1/chat/completions \\
  -H "Authorization: Bearer $SONGGUO_TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${model}",
    "messages": [{ "role": "user", "content": "Hello!" }]
  }'`;

  return (
    <div className={`card ${styles.section}`}>
      <div className={styles.sectionHead}>
        <h3 className={styles.sectionTitle}>Try it</h3>
        <CopyButton value={snippet} label="Copy" />
      </div>
      <p className={styles.sectionHint}>
        Point your client at this gateway and use the model ID as-is. Create a key on the{' '}
        <Link to="/users">Users</Link> page.
      </p>
      <pre className={styles.snippet}>{snippet}</pre>
    </div>
  );
}

function Usage({
  requests,
  errors,
  avgLatency,
}: {
  requests: number;
  errors: number;
  avgLatency: number;
}) {
  return (
    <div className={`card ${styles.section}`}>
      <div className={styles.sectionHead}>
        <h3 className={styles.sectionTitle}>Usage</h3>
      </div>
      {requests === 0 ? (
        <p className={styles.sectionHint}>No traffic yet — send a first request to see stats.</p>
      ) : (
        <div className={styles.usageRow}>
          <div className={styles.fact}>
            <span className={styles.factLabel}>Requests</span>
            <span className={styles.factValue}>{int(requests)}</span>
          </div>
          <div className={styles.fact}>
            <span className={styles.factLabel}>Error rate</span>
            <span className={styles.factValue}>{percent(errors / requests)}</span>
          </div>
          <div className={styles.fact}>
            <span className={styles.factLabel}>Avg latency</span>
            <span className={styles.factValue}>{ms(avgLatency)}</span>
          </div>
        </div>
      )}
    </div>
  );
}
