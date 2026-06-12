// API response types mirroring the Go admin API JSON shapes.

export interface Range {
  since: number;
  until: number;
}

export interface LatencyMS {
  p50: number;
  p95: number;
  p99: number;
}

export interface Overview {
  range: Range;
  total_spend: number;
  spend_by_modality: Record<string, number>;
  requests: number;
  errors: number;
  error_rate: number;
  latency_ms: LatencyMS;
  vendors_active: number;
  users_active: number;
  daily_burn: number;
  runway_days: number | null;
}

export type Bucket = 'hour' | 'day';

export interface SeriesPoint {
  ts: string;
  cost: number;
  requests: number;
  errors: number;
}

export interface UsageSeries {
  bucket: Bucket;
  points: SeriesPoint[];
}

export interface CallEntry {
  id: number;
  ts: string;
  user_id: string;
  model: string;
  modality: string;
  vendor: string;
  credential_id: string;
  /** Matched wire name (e.g. "openai/chat"); "" when no wire matched. */
  wire: string;
  /** Metering trustworthiness: measured | derived | unknown | "". */
  confidence: string;
  attempt: number;
  status: number;
  err: string;
  usage: Record<string, unknown>;
  cost: number;
  latency_ms: number;
  stream: boolean;
  tags: Record<string, string>;
  /** Whether a captured request/response payload exists for this call. */
  has_trace: boolean;
}

/** One side (request or response) of a captured trace. */
export interface TraceSide {
  headers: Record<string, string>;
  body: string;
  /** True when `body` is base64-encoded binary rather than UTF-8 text. */
  body_base64?: boolean;
  content_type: string;
  truncated: boolean;
}

export interface CallTrace {
  call_id: number;
  request: TraceSide;
  response: TraceSide;
  captured_at: string;
}

export interface CallsPage {
  entries: CallEntry[];
  total: number;
  limit: number;
  offset: number;
}

export interface User {
  id: string;
  name: string;
  key_prefix: string;
  budget: number | null;
  scope: string[];
  rpm: number;
  /** Per-user capture override: null = inherit global, true = on, false = off. */
  capture: boolean | null;
  created_at: string;
  revoked_at: string | null;
  spent: number;
  active: boolean;
  /** Plaintext key, present only in the POST /users response. */
  key?: string;
}

export interface CreateUserBody {
  name: string;
  budget?: number | null;
  scope?: string[];
  rpm?: number;
  /** null/omitted inherits global capture; true/false overrides it. */
  capture?: boolean | null;
}

export type PatchUserBody = Partial<
  Pick<User, 'name' | 'budget' | 'scope' | 'rpm' | 'capture'>
>;

export interface Credential {
  id: string;
  masked_key: string;
}

export interface Price {
  input: number;
  output: number;
  unit: string;
}

export interface VendorStats {
  requests: number;
  errors: number;
  error_rate: number;
  avg_latency_ms: number;
  last_status: number;
  healthy: boolean;
}

export interface Vendor {
  name: string;
  base_url: string;
  served_models: string[];
  priority: number;
  weight: number;
  credential: Credential;
  prices: Record<string, Price>;
  stats: VendorStats;
}

export interface VendorTestResult {
  reachable: boolean;
  status: number;
  latency_ms: number;
  error?: string;
}

// --- Services (auto-derived, model-centric view) ---

export interface ServiceProvider {
  id: string;
  name: string;
  priority: number;
  weight: number;
}

export interface ServiceStats {
  requests: number;
  errors: number;
  avg_latency_ms: number;
}

export interface Service {
  model: string;
  providers: ServiceProvider[];
  stats: ServiceStats;
}

// --- Providers (SQLite-backed upstream config) ---

export interface ProviderModel {
  model: string;
  input: number;
  output: number;
  /** Rate for cache-hit input tokens; 0 = no discount (full input rate). */
  cached_input: number;
  unit: string;
}

export interface Provider {
  id: string;
  name: string;
  vendor: string;
  adapter: string;
  base_url: string;
  priority: number;
  weight: number;
  enabled: boolean;
  catalog_id: string;
  /** Enabled wire allowlist; paths matching none are denied. */
  wires: string[];
  /** Forward unmatched paths metered-zero instead of denying them. */
  allow_unmatched: boolean;
  quirks: Record<string, string>;
  /** Masked preview of the provider's API key; "" when no key is set. */
  masked_key: string;
  models: ProviderModel[];
  created_at: string;
  updated_at: string;
  stats: VendorStats;
}

export interface CreateProviderBody {
  name: string;
  vendor?: string;
  adapter: string;
  base_url: string;
  priority?: number;
  weight?: number;
  enabled?: boolean;
  catalog_id?: string;
  allow_unmatched?: boolean;
  quirks?: Record<string, string>;
  api_key?: string;
  models: ProviderModel[];
  wires?: string[];
}

export type PatchProviderBody = Partial<{
  name: string;
  vendor: string;
  adapter: string;
  base_url: string;
  priority: number;
  weight: number;
  enabled: boolean;
  allow_unmatched: boolean;
  quirks: Record<string, string>;
  /** Replaces the provider's API key when present and non-empty. */
  api_key: string;
  models: ProviderModel[];
  wires: string[];
}>;

// --- Catalog (read-only preset directory) ---

export interface CatalogModel {
  model: string;
  input: number;
  output: number;
  cached_input?: number;
  unit: string;
  context?: number;
  modalities?: string[];
}

export interface CatalogService {
  id: string;
  name: string;
  kind: string;
  adapter: string;
  base_url: string;
  docs?: string;
  note?: string;
  wires?: string[];
  quirks?: Record<string, string>;
  models: CatalogModel[];
}

export interface CatalogVendor {
  id: string;
  name: string;
  homepage?: string;
  services: CatalogService[];
}

export interface Catalog {
  vendors: CatalogVendor[];
}

export interface Settings {
  listen: string;
  config_path: string;
  db_path: string;
  admin_protected: boolean;
  version: string;
  watch_mode?: string;
  /** Whether request/response capture is globally enabled. */
  capture: boolean;
  /** Max captured body size in bytes (bodies beyond this are truncated). */
  capture_max_bytes: number;
  /** How many captured payloads are retained. */
  capture_retain: number;
}

export interface PricingRow {
  vendor: string;
  model: string;
  input: number;
  output: number;
  unit: string;
}

export type StatusGroup = 'all' | 'ok' | 'error';

export interface CallsFilters {
  since?: number;
  until?: number;
  user_id?: string;
  model?: string;
  vendor?: string;
  status?: StatusGroup;
  limit?: number;
  offset?: number;
}
