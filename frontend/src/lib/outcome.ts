// What actually happened to a call.
//
// The HTTP status alone cannot answer that. songguo mints statuses of its own —
// a budget refusal is a 402, an unmatched wire is a 404, a routing miss is a 502
// — so on the status int a denial we issued is indistinguishable from the same
// code coming back from the vendor. The pair (status, err) *is* unambiguous:
// every forwarded call carries err == "", and every gateway-origin outcome
// carries a slug written by denyCapture. So the outcome is derived from both,
// never from the status alone.
//
// Deriving rather than persisting is deliberate: it is correct for all existing
// history the moment it ships, whereas a new column would be empty on every row
// already in the ledger.

/**
 * Slugs the gateway writes into `err`. Mirrors the exported constants in
 * backend/internal/calls — keep the two in step; `outcomeOf` has a table test
 * asserting every slug maps to something other than `unknown`.
 */
export const ERR_SLUGS = {
  budgetExceeded: 'budget_exceeded',
  rateLimited: 'rate_limited',
  clientGone: 'client_gone',
  modelNotAllowed: 'model_not_allowed',
  vendorNotAllowed: 'vendor_not_allowed',
  noUpstream: 'no_upstream',
  requestBuildFailed: 'request_build_failed',
  /** Legacy: covered both transport and request-build failure before they split. */
  upstreamError: 'upstream_error',
  /** Prefixes — the slug is followed by ": <detail>". */
  unmatchedPrefix: 'unmatched:',
  transportPrefix: 'transport_error:',
  streamPrefix: 'stream_error:',
} as const;

export type Outcome =
  | 'in_flight'
  | 'abandoned'
  | 'ok'
  | 'vendor_error'
  | 'truncated'
  | 'transport_error'
  | 'upstream_failed'
  | 'no_route'
  | 'build_failed'
  | 'unmatched'
  | 'denied_budget'
  | 'denied_rate'
  | 'denied_scope'
  | 'client_gone';

/** The subset of a call entry `outcomeOf` needs. */
export interface OutcomeInput {
  status: number;
  err: string;
  pending: boolean;
  /**
   * True when the row was created before the current gateway process booted, so
   * no live request owns it. Supplied by the backend, which is the only side
   * that knows its own boot time.
   */
  abandoned?: boolean;
}

/**
 * Classify a call. Total over every (status, err) the gateway can write — the
 * `unknown` fallback exists only so an unrecognized future slug degrades to a
 * visible "we don't know" rather than being silently filed as an error.
 */
export function outcomeOf(e: OutcomeInput): Outcome | 'unknown' {
  if (e.pending) return e.abandoned ? 'abandoned' : 'in_flight';

  const err = e.err ?? '';
  if (err === '') {
    // Legacy rows: before the slugs existed, status 0 with no detail was the
    // documented encoding for "no upstream response". Nothing writes it now.
    // Reading it as a transport failure decodes an old convention rather than
    // guessing — the old code and docs agree on what it meant.
    if (e.status === 0) return 'transport_error';
    // Forwarded: the vendor answered and the status is entirely theirs.
    return e.status >= 400 ? 'vendor_error' : 'ok';
  }

  if (err.startsWith(ERR_SLUGS.transportPrefix)) return 'transport_error';
  if (err.startsWith(ERR_SLUGS.streamPrefix)) return 'truncated';
  if (err.startsWith(ERR_SLUGS.unmatchedPrefix)) return 'unmatched';

  switch (err) {
    case ERR_SLUGS.clientGone:
      return 'client_gone';
    case ERR_SLUGS.budgetExceeded:
      return 'denied_budget';
    case ERR_SLUGS.rateLimited:
      return 'denied_rate';
    case ERR_SLUGS.modelNotAllowed:
    case ERR_SLUGS.vendorNotAllowed:
      return 'denied_scope';
    case ERR_SLUGS.noUpstream:
      return 'no_route';
    case ERR_SLUGS.requestBuildFailed:
      return 'build_failed';
    case ERR_SLUGS.upstreamError:
      // Legacy rows only. This slug meant transport failure OR request-build
      // failure and we cannot now tell which, so it gets its own value rather
      // than masquerading as a precisely classified transport error.
      return 'upstream_failed';
    default:
      return 'unknown';
  }
}

/**
 * Pill styling class. Colored by *who* the row is about, so the scheme is
 * learnable at a glance: green served, amber the provider rejected the request,
 * red the provider failed, violet songguo refused it, grey no verdict.
 */
export type OutcomeTone = 'ok' | 'warn' | 'err' | 'gateway' | 'idle';

interface OutcomeStyle {
  /** Short label. Empty means "show the raw status code instead". */
  label: string;
  tone: OutcomeTone;
  /** One line explaining what the label means, for a title tooltip. */
  hint: string;
}

const STYLES: Record<Outcome | 'unknown', OutcomeStyle> = {
  in_flight: { label: 'In flight', tone: 'idle', hint: 'Request is still running; no result yet.' },
  abandoned: {
    label: 'Never finished',
    tone: 'idle',
    // Deliberately not "crashed": a crash, a clean SIGTERM and a docker stop
    // mid-call all leave exactly this row. Naming one of them would be a guess.
    hint: 'The gateway restarted before this call finished. No result was ever recorded.',
  },
  ok: { label: '', tone: 'ok', hint: 'The provider answered.' },
  vendor_error: { label: '', tone: 'err', hint: "The provider's own error response, forwarded unchanged." },
  truncated: { label: 'Stream cut', tone: 'err', hint: 'The provider began answering, then the stream broke mid-body.' },
  transport_error: { label: 'No response', tone: 'err', hint: 'The provider never answered — the connection failed.' },
  upstream_failed: { label: 'Upstream failed', tone: 'err', hint: 'The call never reached the provider. Older rows do not record which step failed.' },
  no_route: { label: 'No route', tone: 'err', hint: 'No provider is configured to serve this model.' },
  build_failed: { label: 'Build failed', tone: 'err', hint: 'The gateway could not construct the upstream request.' },
  unmatched: { label: 'No wire', tone: 'gateway', hint: 'No wire mapping matches this endpoint.' },
  denied_budget: { label: 'Budget', tone: 'gateway', hint: "Refused by songguo: the user's budget is spent." },
  denied_rate: { label: 'Rate limit', tone: 'gateway', hint: "Refused by songguo's own rate limit — not the provider's." },
  denied_scope: { label: 'Not allowed', tone: 'gateway', hint: 'Refused by songguo: this key may not use that model or provider.' },
  client_gone: { label: 'Client left', tone: 'idle', hint: 'The caller disconnected before the answer arrived. Not a provider failure.' },
  unknown: { label: 'Unknown', tone: 'err', hint: 'Unrecognized outcome — the gateway recorded an error slug this dashboard does not know.' },
};

export function outcomeStyle(o: Outcome | 'unknown'): OutcomeStyle {
  return STYLES[o] ?? STYLES.unknown;
}

/**
 * Pill tone. Status-aware for `vendor_error` alone: a provider rejecting the
 * request (4xx) and a provider breaking (5xx) are different events, and the
 * status already draws that line — so the color follows it rather than
 * duplicating it in a second field that could disagree.
 */
export function outcomeTone(o: Outcome | 'unknown', status: number): OutcomeTone {
  if (o === 'vendor_error') return status >= 500 ? 'err' : 'warn';
  return outcomeStyle(o).tone;
}

/**
 * What the pill reads. `ok` and `vendor_error` show the provider's real status
 * code, because that code IS the answer; everything else names what happened,
 * since its status was minted by the gateway and would be misleading alone.
 *
 * The sentinel guard is belt-and-braces: no outcome that renders a raw code can
 * legitimately carry -1 or 0, but making it structurally impossible to print one
 * beats relying on that never being wrong.
 */
export function outcomeLabel(o: Outcome | 'unknown', status: number): string {
  const label = outcomeStyle(o).label;
  if (label !== '') return label;
  return isRealHTTPStatus(status) ? String(status) : STYLES.unknown.label;
}

/** True for a code a server actually sent, as opposed to one of our sentinels. */
export function isRealHTTPStatus(status: number): boolean {
  return Number.isFinite(status) && status >= 100 && status < 600;
}

/**
 * Who the outcome is about. Kept separate from `tone` on purpose: tone is how
 * the pill looks, blame is what the row means, and they genuinely differ —
 * `no_route` is red but is our misconfiguration, never the provider's fault.
 */
export type Blame = 'provider' | 'gateway' | 'caller' | 'none';

const BLAME: Record<Outcome | 'unknown', Blame> = {
  // The provider answered, or failed to.
  ok: 'provider',
  vendor_error: 'provider',
  truncated: 'provider',
  transport_error: 'provider',
  // songguo's own doing.
  no_route: 'gateway',
  build_failed: 'gateway',
  unmatched: 'gateway',
  denied_budget: 'gateway',
  denied_rate: 'gateway',
  denied_scope: 'gateway',
  // The caller's.
  client_gone: 'caller',
  // No verdict is available.
  in_flight: 'none',
  abandoned: 'none',
  // Legacy `upstream_error` covered a transport failure (provider) and a
  // request-build failure (gateway) alike. Guessing between them is exactly
  // what this module exists to stop, so it is charged to neither.
  upstream_failed: 'none',
  unknown: 'none',
};

export function blameFor(o: Outcome | 'unknown'): Blame {
  return BLAME[o] ?? 'none';
}

/**
 * True when the outcome counts against a provider's success rate. In-flight work
 * has no verdict yet, a caller hanging up is the caller's business, and
 * songguo's own refusals are ours — none of the three is the vendor failing.
 */
export function isProviderFailure(o: Outcome | 'unknown'): boolean {
  return blameFor(o) === 'provider' && o !== 'ok';
}

/**
 * True when songguo refused the call under a limit the operator configured —
 * the gateway working, not failing. Mirrors `calls.IsPolicyDenial` in Go; keep
 * the two lists in step.
 *
 * Such a call never reached a provider, so it says something about a budget and
 * nothing about whether requests are being served: it belongs in neither side of
 * a success rate, exactly like an in-flight call or one the caller abandoned.
 *
 * Narrower than `blameFor(o) === 'gateway'` on purpose. `no_route`,
 * `build_failed` and `unmatched` are also ours, but they are misconfigurations
 * that have to stay visible as failures. So is `denied_scope`: a budget resets
 * and a rate window passes, but a caller asking for a model its key does not
 * carry fails identically forever, and no other panel would report it.
 */
export function isPolicyDenial(o: Outcome | 'unknown'): boolean {
  return o === 'denied_budget' || o === 'denied_rate';
}
