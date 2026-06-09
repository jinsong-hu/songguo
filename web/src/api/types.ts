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
  tokens_active: number;
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
  token_id: string;
  model: string;
  modality: string;
  vendor: string;
  credential_id: string;
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

export interface Token {
  id: string;
  name: string;
  key_prefix: string;
  budget: number | null;
  scope: string[];
  rpm: number;
  /** Per-token capture override: null = inherit global, true = on, false = off. */
  capture: boolean | null;
  created_at: string;
  revoked_at: string | null;
  spent: number;
  active: boolean;
  /** Plaintext key, present only in the POST /tokens response. */
  key?: string;
}

export interface CreateTokenBody {
  name: string;
  budget?: number | null;
  scope?: string[];
  rpm?: number;
  /** null/omitted inherits global capture; true/false overrides it. */
  capture?: boolean | null;
}

export type PatchTokenBody = Partial<
  Pick<Token, 'name' | 'budget' | 'scope' | 'rpm' | 'capture'>
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
  credentials: Credential[];
  prices: Record<string, Price>;
  stats: VendorStats;
}

export interface VendorTestResult {
  reachable: boolean;
  status: number;
  latency_ms: number;
  error?: string;
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
  token_id?: string;
  model?: string;
  vendor?: string;
  status?: StatusGroup;
  limit?: number;
  offset?: number;
}
