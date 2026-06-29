import { ArrowRight, Boxes, Braces, Info } from 'lucide-react';
import { Link } from 'react-router-dom';
import { CopyButton } from '../components/CopyButton';
import { Page } from '../components/Layout';
import styles from './DocsHome.module.css';

// Landing page for the Docs section: a short orientation plus links into the two
// reference pages (the OpenAI/Anthropic-shaped data plane, and the MCP control
// plane). Consumers authenticate with a Songguo user key as a bearer token.
export function DocsHomePage() {
  const base = `${window.location.origin}/v1`;

  return (
    <Page title="Docs">
      <div className={styles.sections}>
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Calling the gateway</div>
          <div className={styles.panelDesc}>
            Songguo is a transparent proxy: keep your existing OpenAI- or
            Anthropic-shaped SDK and change two things — the base URL, and the API
            key. The <code>model</code> string selects the provider; Songguo swaps
            in the real upstream credential.
          </div>
          <div className={styles.endpoint}>
            <code className={styles.endpointUrl}>{base}</code>
            <CopyButton value={base} label="Copy" />
          </div>
          <div className={styles.hint}>
            <Info size={14} />
            Mint user keys on the <Link to="/users">Users</Link> page; browse
            routable models on <Link to="/services">Services</Link>.
          </div>
        </div>

        <div className={styles.cards}>
          <Link to="/docs/api" className={`card ${styles.linkCard}`}>
            <Braces size={18} />
            <div className={styles.linkBody}>
              <div className={styles.linkTitle}>API Reference</div>
              <div className={styles.linkDesc}>
                The OpenAPI contract for the admin and data-plane endpoints.
              </div>
            </div>
            <ArrowRight size={16} className={styles.linkArrow} />
          </Link>

          <Link to="/docs/mcp" className={`card ${styles.linkCard}`}>
            <Boxes size={18} />
            <div className={styles.linkBody}>
              <div className={styles.linkTitle}>MCP</div>
              <div className={styles.linkDesc}>
                Drive the control plane from an agent over Model Context Protocol.
              </div>
            </div>
            <ArrowRight size={16} className={styles.linkArrow} />
          </Link>
        </div>
      </div>
    </Page>
  );
}
