import { ApiReferenceReact } from '@scalar/api-reference-react';
// Scalar's stylesheet is imported transitively by the component, but that import
// gets tree-shaken out of the production build — import it explicitly so the
// reference is actually styled.
import '@scalar/api-reference-react/style.css';
import { CopyButton } from '../components/CopyButton';
import { useTheme } from '../lib/useTheme';
import styles from './DocsApi.module.css';

// The backend serves the OpenAPI 3.1 contract at /openapi.yaml (unauthenticated,
// schema only). Scalar renders it; we bind its dark mode to the app theme and
// disable telemetry so a self-hosted gateway never phones home.
export function DocsApiPage() {
  const { resolvedTheme } = useTheme();
  const specUrl = `${window.location.origin}/openapi.yaml`;

  return (
    <div className={styles.wrap}>
      <div className={styles.bar}>
        <h1 className={styles.title}>API Reference</h1>
        <div className={styles.spec}>
          <span className={styles.specLabel}>OpenAPI spec</span>
          <code className={styles.specUrl}>{specUrl}</code>
          <CopyButton value={specUrl} label="Copy" />
        </div>
      </div>
      <div className={styles.scalar}>
        <ApiReferenceReact
          configuration={{
            url: '/openapi.yaml',
            theme: 'default',
            layout: 'modern',
            forceDarkModeState: resolvedTheme,
            hideDarkModeToggle: true,
            telemetry: false,
          }}
        />
      </div>
    </div>
  );
}
