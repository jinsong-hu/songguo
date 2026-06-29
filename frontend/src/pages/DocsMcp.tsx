import { Info, Lock, LockOpen } from 'lucide-react';
import { CopyButton } from '../components/CopyButton';
import { Page } from '../components/Layout';
import { useSettings } from '../lib/settingsContext';
import styles from './DocsMcp.module.css';

// Tool catalogue mirrors backend/internal/api/mcp.go. Read tools are always
// registered; write tools are registered only when SONGGUO_MCP_WRITE=1.
const READ_TOOLS: ReadonlyArray<[string, string]> = [
  ['get_overview', 'Spend summary for a time window: total spend, spend by modality, request/error counts, latency percentiles, active providers/users, daily burn and runway. Defaults to the last 30 days.'],
  ['get_usage_series', 'Cost, request and error totals bucketed over time for plotting trends. Defaults to the last 7 days; bucket is hour or day.'],
  ['list_calls', 'Browse individual gateway calls (the per-request ledger), newest first, with filters by user, model, vendor, status and time. Returns entries plus total count.'],
  ['get_call_trace', 'Return the captured request/response payload for one call id (only when capture is enabled for that call).'],
  ['list_users', 'List all gateway users (consumer keys) with budget, scope, RPM limit, lifetime spend and active state. Plaintext keys are never returned.'],
  ['list_providers', 'List all configured upstream providers with their wire endpoints, models/prices, quirks and health stats. API keys are masked.'],
  ['list_services', 'List the auto-derived, model-centric services: each unique model name and the providers behind it, with aggregate call stats.'],
  ['list_pricing', 'List every per-provider model price (input, output, unit) currently configured.'],
  ['get_settings', 'Return non-secret runtime settings: listen address, db path, whether the admin API is protected, version, and capture configuration.'],
];

const WRITE_TOOLS: ReadonlyArray<[string, string]> = [
  ['create_user', 'Create a gateway user (consumer key). Returns the plaintext key once. Fields: name (required), budget, scope, rpm.'],
  ['update_user', 'Update a user’s mutable fields via a patch object (name, budget, scope, rpm).'],
  ['revoke_user', 'Revoke a user by id, immediately disabling its key.'],
  ['create_provider', 'Create an upstream provider: name, vendor, api_key, priority, weight, enabled, quirks, models and wire endpoints.'],
  ['update_provider', 'Update a provider’s mutable fields. Supplying models or endpoints replaces those lists wholesale.'],
  ['delete_provider', 'Delete a provider by id; services it backed are re-derived without it.'],
  ['update_settings', 'Update the capture setting: capture on/off. Returns the resulting settings.'],
  ['test_provider', 'Probe a provider’s host for reachability using its API key. Returns reachability, status and latency.'],
];

export function DocsMcpPage() {
  const { settings } = useSettings();
  const mcpUrl = `${window.location.origin}/mcp`;
  const protectedApi = settings.admin_protected;

  const clientConfig = JSON.stringify(
    {
      mcpServers: {
        songguo: {
          type: 'http',
          url: mcpUrl,
          ...(protectedApi
            ? { headers: { Authorization: 'Bearer <SONGGUO_ADMIN_KEY>' } }
            : {}),
        },
      },
    },
    null,
    2,
  );

  return (
    <Page title="MCP">
      <div className={styles.sections}>
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Model Context Protocol</div>
          <div className={styles.panelDesc}>
            Songguo exposes its control plane — the same surface as the admin API —
            as an MCP server, so an agent can read usage and manage the gateway
            with native tool calls instead of raw HTTP.
          </div>

          <div className={styles.kv}>
            <span className={styles.kvKey}>Endpoint</span>
            <div className={styles.endpoint}>
              <code className={styles.endpointUrl}>{mcpUrl}</code>
              <CopyButton value={mcpUrl} label="Copy" />
            </div>

            <span className={styles.kvKey}>Transport</span>
            <span className={styles.kvVal}>Stateless streamable HTTP</span>

            <span className={styles.kvKey}>Auth</span>
            <span>
              <span
                className={`${styles.badge} ${
                  protectedApi ? styles.badgeProtected : styles.badgeOpen
                }`}
              >
                {protectedApi ? <Lock size={11} /> : <LockOpen size={11} />}
                {protectedApi ? 'Admin bearer key required' : 'Open (no key)'}
              </span>
            </span>
          </div>
        </div>

        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Connect a client</div>
          <div className={styles.panelDesc}>
            Point any MCP client at the endpoint over streamable HTTP.
            {protectedApi
              ? ' Send the admin key as a bearer token.'
              : ' No credentials are required while the admin API is unprotected.'}
          </div>
          <div className={styles.codeBlock}>
            <CopyButton value={clientConfig} label="Copy" className={styles.codeCopy} />
            <pre className={styles.code}>{clientConfig}</pre>
          </div>
        </div>

        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Read tools</div>
          <div className={styles.panelDesc}>
            Always available. Read-only views over usage, the call ledger, users,
            providers, services and settings.
          </div>
          <ToolTable tools={READ_TOOLS} />
        </div>

        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Write tools</div>
          <div className={styles.panelDesc}>
            Registered only when the server runs with <code>SONGGUO_MCP_WRITE=1</code>.
            The admin key already grants full control, so write access is opt-in
            rather than implicit.
          </div>
          <ToolTable tools={WRITE_TOOLS} />
        </div>

        <div className={styles.hint}>
          <Info size={14} />
          Tool behavior is identical to the matching admin-API endpoints — see the
          API Reference for request and response shapes.
        </div>
      </div>
    </Page>
  );
}

function ToolTable({ tools }: { tools: ReadonlyArray<[string, string]> }) {
  return (
    <div className={styles.tableScroll}>
      <table className="table">
        <thead>
          <tr>
            <th>Tool</th>
            <th>Description</th>
          </tr>
        </thead>
        <tbody>
          {tools.map(([name, desc]) => (
            <tr key={name}>
              <td className="mono">{name}</td>
              <td>{desc}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
