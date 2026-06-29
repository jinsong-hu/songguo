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
  created_at: string;
  revoked_at: string | null;
  spent: number;
  active: boolean;
  /** RFC3339 timestamp of the user's most recent call, or null if never used. */
  last_seen: string | null;
  /** Plaintext key. Empty for users created before key storage existed. */
  key?: string;
}

export interface CreateUserBody {
  name: string;
  budget?: number | null;
  scope?: string[];
  rpm?: number;
}

export type PatchUserBody = Partial<
  Pick<User, 'name' | 'budget' | 'scope' | 'rpm'>
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
  origin: string;
  endpoints: Record<string, string>;
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

/** One wire bound to its full upstream URL + adapter (auth scheme); 1:1 with the wire. */
export interface ProviderEndpoint {
  wire: string;
  endpoint: string;
  adapter: string;
}

export interface Provider {
  id: string;
  name: string;
  vendor: string;
  priority: number;
  weight: number;
  enabled: boolean;
  catalog_id: string;
  /** Configured endpoints; each binds one wire to its full upstream URL + adapter. */
  endpoints: ProviderEndpoint[];
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
  priority?: number;
  weight?: number;
  enabled?: boolean;
  catalog_id?: string;
  allow_unmatched?: boolean;
  quirks?: Record<string, string>;
  api_key?: string;
  models: ProviderModel[];
  endpoints: ProviderEndpoint[];
}

export type PatchProviderBody = Partial<{
  name: string;
  vendor: string;
  priority: number;
  weight: number;
  enabled: boolean;
  allow_unmatched: boolean;
  quirks: Record<string, string>;
  /** Replaces the provider's API key when present and non-empty. */
  api_key: string;
  models: ProviderModel[];
  endpoints: ProviderEndpoint[];
}>;

// --- Catalog (read-only preset directory) ---

export interface CatalogModel {
  input: number;
  output: number;
  cached_input?: number;
  unit: string;
  context?: number;
  modalities?: string[];
}

/** A preset wire bound to its full upstream URL + adapter, with the model ids it serves. */
export interface CatalogEndpoint {
  wire: string;
  endpoint: string;
  adapter: string;
  docs?: string;
  note?: string;
  models?: string[];
}

export interface CatalogVendor {
  id: string;
  name: string;
  homepage?: string;
  quirks?: Record<string, string>;
  /** Template vendor: no preset models, user supplies base URL ({base} placeholder) and model ids. */
  custom?: boolean;
  /** Price list keyed by model id, shared across this vendor's endpoints. */
  models: Record<string, CatalogModel>;
  endpoints: CatalogEndpoint[];
}

export interface Catalog {
  vendors: CatalogVendor[];
}

export interface Settings {
  listen: string;
  db_path: string;
  admin_protected: boolean;
  version: string;
  /** Whether request/response capture is globally enabled. */
  capture: boolean;
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
