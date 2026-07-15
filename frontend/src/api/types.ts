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

export interface TokenTotals {
  /** Fresh (uncached) input tokens. input, cached, and cache_creation are disjoint. */
  input: number;
  output: number;
  /** Cache-read input tokens (disjoint from input). */
  cached: number;
  /** Cache-write input tokens (disjoint from input). */
  cache_creation: number;
  /** Reasoning/thinking tokens (subset of output). */
  thinking: number;
}

export interface Overview {
  range: Range;
  total_spend: number;
  spend_by_modality: Record<string, number>;
  tokens: TokenTotals;
  requests: number;
  errors: number;
  error_rate: number;
  latency_ms: LatencyMS;
  ttft_ms: LatencyMS;
  output_tokens_per_second: LatencyMS;
  vendors_active: number;
  users_active: number;
  /** Distinct users with traffic in the window. */
  active_callers: number;
  daily_burn: number;
  runway_days: number | null;
}

export type Bucket = 'hour' | 'day';

/**
 * Aggregate stats over coding-agent sessions in a window (GET /api/sessions/overview).
 * Outcome (completed/errored/interrupted) is inferred from each session's last
 * call — an interaction-level signal off the ledger, not a judgment on the
 * coding task. Durations are in seconds.
 */
export interface SessionStats {
  range: Range;
  sessions: number;
  completed: number;
  errored: number;
  interrupted: number;
  /** Sessions that spawned at least one subagent. */
  with_subagents: number;
  total_turns: number;
  total_tokens: number;
  total_tool_calls: number;
  avg_turns: number;
  avg_tokens: number;
  avg_duration: number;
  avg_tool_calls: number;
  turns_p50: number;
  turns_p95: number;
  tokens_p50: number;
  tokens_p95: number;
  duration_p50: number;
  duration_p95: number;
  tool_calls_p50: number;
  tool_calls_p95: number;
}

export interface SeriesPoint {
  ts: string;
  cost: number;
  requests: number;
  errors: number;
  /** Fresh (uncached) input tokens. */
  input_tokens: number;
  output_tokens: number;
  /** Cache-read input tokens (disjoint from input_tokens). */
  cache_read_input_tokens: number;
  /** Cache-write input tokens (disjoint from input_tokens). */
  cache_creation_input_tokens: number;
  /** Reasoning/thinking tokens (subset of output_tokens). */
  thinking_tokens: number;
  avg_latency_ms: number;
  avg_ttft_ms: number;
  avg_output_tokens_per_second: number;
}

export interface UsageSeries {
  bucket: Bucket;
  points: SeriesPoint[];
}

export interface TokensByModelPoint {
  ts: string;
  cost: number;
  tokens: Record<string, number>;
  costs: Record<string, number>;
  /** Per-key average time-to-first-token (ms). Same key set as `tokens`. */
  ttft: Record<string, number>;
  /** Per-key average output throughput (tokens/sec). Same key set as `tokens`. */
  tps: Record<string, number>;
}

// Dimension the Usage stacked charts group their series by. "vendor" is
// surfaced as "provider" in the UI; the underlying calls column is `vendor`.
export type UsageDimension = 'model' | 'vendor' | 'user';

export interface TokensByModelSeries {
  bucket: Bucket;
  // Series keys for the current dimension (model ids, vendor names, or user
  // ids), top-N + "Other". Named `models` for historical reasons.
  models: string[];
  points: TokensByModelPoint[];
}

// One bucket of the success-by-model series: request and error counts keyed by
// dimension key. `requests` and `errors` carry the same key set; success % is
// derived as (requests - errors) / requests.
export interface SuccessByModelPoint {
  ts: string;
  requests: Record<string, number>;
  errors: Record<string, number>;
}

export interface SuccessByModelSeries {
  bucket: Bucket;
  // Series keys for the current dimension (top-N by requests + "Other").
  // Named `models` to mirror TokensByModelSeries.
  models: string[];
  points: SuccessByModelPoint[];
}

// One bucket of the cache-by-model series: cache-read and total-input token sums
// keyed by dimension key. `cache_read` and `input` carry the same key set; the
// cache-hit ratio is derived as cache_read / input (total input = fresh + cache
// read + cache creation).
export interface CacheByModelPoint {
  ts: string;
  cache_read: Record<string, number>;
  input: Record<string, number>;
}

export interface CacheByModelSeries {
  bucket: Bucket;
  // Series keys for the current dimension (top-N by total input + "Other").
  // Named `models` to mirror TokensByModelSeries.
  models: string[];
  points: CacheByModelPoint[];
}

export type BreakdownDimension = 'model' | 'vendor' | 'user' | 'modality';

export interface BreakdownRow {
  key: string;
  requests: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
  thinking_tokens: number;
  cost: number;
  avg_latency_ms: number;
}

export interface Breakdown {
  range: Range;
  dimension: string;
  rows: BreakdownRow[];
}

export interface ErrorBreakdown {
  range: Range;
  rate_limited: number;
  client_error: number;
  server_error: number;
  transport: number;
}

// One upstream status code and its error-row count. status 0 = transport failure
// (no response); otherwise the raw HTTP status (429, 500, 503, …).
export interface ErrorCodeRow {
  status: number;
  count: number;
}

// GET /usage/error-codes: error rows grouped by status, ranked by count desc.
export interface ErrorCodesBreakdown {
  range: Range;
  rows: ErrorCodeRow[];
}

export interface CallEntry {
  /** UUID minted by the gateway at request-start. */
  id: string;
  ts: string;
  /** End time; absent while the call is still in flight (pending). */
  ts_end?: string;
  /** True when the call was created but not yet finalized (in flight). */
  pending: boolean;
  user_id: string;
  model: string;
  modality: string;
  vendor: string;
  credential_id: string;
  /** Matched wire name (e.g. "openai/chat"); "" when no wire matched. */
  wire: string;
  /** Metering trustworthiness: measured | derived | unknown | "". */
  confidence: string;
  status: number;
  err: string;
  usage: Record<string, unknown>;
  cost: number;
  /** Fresh (uncached) input tokens. */
  input_tokens: number;
  output_tokens: number;
  /** Cache-read input tokens (disjoint from input_tokens). */
  cache_read_input_tokens: number;
  /** Cache-write input tokens (disjoint from input_tokens). */
  cache_creation_input_tokens: number;
  /** Reasoning/thinking tokens (subset of output_tokens). */
  thinking_tokens: number;
  latency_ms: number;
  ttft_ms: number;
  generation_ms: number;
  output_tokens_per_second: number;
  stream: boolean;
  tags: Record<string, string>;
  /** Normalized caller client parsed from User-Agent, e.g. claude-code. */
  client_name: string;
  client_version: string;
  /** Coding-agent attribution (empty for ordinary API traffic). */
  session_id: string;
  agent_id: string;
  parent_agent_id: string;
  /** Why the call was made: "main" or a harness utility kind (monitor | count_tokens | utility). Empty legacy rows = main. */
  entrypoint: string;
  /** Whether a captured request/response payload exists for this call. */
  has_trace: boolean;
  /** Single-request context-window composition, present when the request was decomposable. */
  composition?: RequestComposition;
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
  call_id: string;
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

/**
 * One row of the activity feed: either an aggregated coding-agent session
 * (kind === "session") or a standalone request (kind === "request"). Fields not
 * relevant to a kind are absent/zero.
 */
export interface FeedRow {
  kind: 'session' | 'request';
  /** Set for session rows — captured coding-agent session id to link to. */
  session_id?: string;
  /** Durable title derived from captured session traffic. */
  title?: string;
  /** Set for request rows — the call id (UUID) to link to. */
  request_id?: string;
  calls: number;
  cost: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
  /** Billed audio duration in seconds (ASR wires); 0 for token-metered rows. */
  seconds: number;
  /** Billed text length in characters (TTS wires); 0 for token-metered rows. */
  chars: number;
  first_ts: string;
  last_ts: string;
  /** Duration from first request start to last request end, in milliseconds. */
  duration_ms: number;
  error_count: number;
  /** Model with the most calls in this feed row; useful for aggregated sessions. */
  major_model?: string;
  models: string[];
  vendors: string[];
  // Single-call fields, present only on request rows.
  model?: string;
  vendor?: string;
  wire?: string;
  confidence?: string;
  modality?: string;
  status?: number;
  latency_ms?: number;
  stream?: boolean;
}

export interface FeedPage {
  rows: FeedRow[];
  total: number;
  limit: number;
  offset: number;
}

/** One node in a session's agent tree; rollups cover the whole subtree. */
export interface AgentNode {
  agent_id: string;
  calls: number;
  cost: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
  thinking_tokens: number;
  children: AgentNode[];
}

/** GET /api/sessions/{id}: session rollups, agent tree, and its calls. */
export interface SessionDetail {
  session_id: string;
  title?: string;
  calls: number;
  cost: number;
  /** Fresh (uncached) input tokens summed across the session's calls. */
  input_tokens: number;
  output_tokens: number;
  /** Cache-read input tokens (disjoint from input_tokens). */
  cache_read_input_tokens: number;
  /** Cache-write input tokens (disjoint from input_tokens). */
  cache_creation_input_tokens: number;
  /** Reasoning/thinking tokens (subset of output_tokens). */
  thinking_tokens: number;
  error_count: number;
  first_ts: string;
  last_ts: string;
  models: string[];
  vendors: string[];
  agents: AgentNode[];
  entries: CallEntry[];
}

/** GET /api/sessions/{id}/messages: compact, de-duplicated prompt material. */
export interface SessionMessages {
  session_id: string;
  model: string;
  /** Unique request-level system/instructions values in first-seen order. */
  system: unknown[];
  /** Unique raw tool definitions in first-seen order. */
  tools: unknown[];
  /** Raw message/input items after cumulative-history overlap removal. */
  messages: unknown[];
}

// --- Context distribution (where the context window goes) ---

/** A producer sub-slice of a source bucket (e.g. Read under tool results). */
export interface ProducerSlice {
  /** e.g. "read" | "bash" | "grep" | "mcp:chrome" | "task" | "skill" | "builtin" | "claude_md". */
  key: string;
  tokens: number;
}

/** One top-level source bucket of the context window. */
export interface SourceSlice {
  /** "tool_results" | "tool_schemas" | "system" | "reasoning" | "actions" | "user" | "attachments". */
  key: string;
  tokens: number;
  /** Portion of this slice served from cache (cache_read) — for the cache cross-cut. */
  cached: number;
  /** Producer drill-down; present for tool_results and tool_schemas. */
  children?: ProducerSlice[];
}

export interface ContextDistribution {
  /** Requests contributing to this local aggregate. */
  requests: number;
  /** Mean full-window tokens per request in the aggregate. */
  avg_total: number;
  sources: SourceSlice[];
  blocks?: ContextBlock[];
}

/** Itemized context block aggregated from the same local counter as sources. */
export interface ContextBlock {
  source: string;
  producer?: string;
  type: string;
  hash: string;
  /** Average tokens per occurrence. */
  tokens: number;
  cached: number;
  occurrences: number;
  /** Exact summed tokens across occurrences. */
  total: number;
}

export interface RequestComposition {
  total: number;
  cached: number;
  sources: SourceSlice[];
}

/** GET /api/context/composition: aggregated window composition over a range. */
export interface ContextComposition {
  range: Range;
  /** Requests contributing to the aggregate. */
  requests: number;
  /** Mean full-window tokens (input + cache_read + cache_creation) per request. */
  avg_total: number;
  sources: SourceSlice[];
}

/** Per-turn context snapshot for the session growth chart. */
export interface ContextTurn {
  call_id: string;
  seq: number;
  ts: string;
  agent_id: string;
  total: number;
  cached: number;
  /** Tokens per source key. */
  sources: Record<string, number>;
}

/** A resident block ranked by dwell (tokens × turns resident). */
export interface DwellBlock {
  label: string;
  producer: string;
  tokens: number;
  turns: number;
  dwell: number;
}

/** GET /api/sessions/{id}/context: growth series, snapshot, dwell. */
export interface SessionContext {
  session_id: string;
  title?: string;
  turns: ContextTurn[];
  /** Request-weighted aggregate context distribution for the whole session. */
  distribution: ContextDistribution;
  /** Latest-window composition tree (snapshot). */
  snapshot: SourceSlice[];
  /** Top resident blocks by dwell (empty until lineage tracking lands). */
  dwell: DwellBlock[];
}

export interface User {
  id: string;
  name: string;
  key_prefix: string;
  budget: number | null;
  scope: string[];
  rpm: number;
  capture: boolean;
  created_at: string;
  revoked_at: string | null;
  spent: number;
  active: boolean;
  /** RFC3339 timestamp of the user's most recent call, or null if never used. */
  last_seen: string | null;
  /** Plaintext key. Empty for users created before key storage existed. */
  key?: string;
}

/** Whoami: who the signed-in key belongs to. `admin` unlocks the full
 *  dashboard; `user` (a consumer key) unlocks the scoped playground only. */
export interface Me {
  role: 'admin' | 'user';
  id: string;
  name: string;
  /** Models the key may play; empty means all. */
  scope: string[];
  budget: number | null;
  spend: number;
}

export interface CreateUserBody {
  name: string;
  budget?: number | null;
  scope?: string[];
  rpm?: number;
  capture?: boolean;
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
  /** True when this row intentionally overrides catalog pricing. */
  price_override?: boolean;
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
