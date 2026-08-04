import { describe, expect, it } from 'vitest';
import {
  ERR_SLUGS,
  blameFor,
  isPolicyDenial,
  isProviderFailure,
  outcomeLabel,
  outcomeOf,
  outcomeStyle,
  outcomeTone,
  type Outcome,
  type OutcomeInput,
} from './outcome';

const call = (o: Partial<OutcomeInput> = {}): OutcomeInput => ({
  status: 200,
  err: '',
  pending: false,
  ...o,
});

const PENDING = -1;

describe('outcomeOf', () => {
  it('separates a running call from one the gateway never finished', () => {
    // The whole point: -1 alone cannot tell these apart, so the backend supplies
    // the one bit it knows and we never guess.
    expect(outcomeOf(call({ status: PENDING, pending: true }))).toBe('in_flight');
    expect(outcomeOf(call({ status: PENDING, pending: true, abandoned: true }))).toBe('abandoned');
  });

  it('reads a forwarded call as the provider answering, whatever the code', () => {
    expect(outcomeOf(call({ status: 200 }))).toBe('ok');
    expect(outcomeOf(call({ status: 204 }))).toBe('ok');
    expect(outcomeOf(call({ status: 404 }))).toBe('vendor_error');
    expect(outcomeOf(call({ status: 503 }))).toBe('vendor_error');
  });

  it("distinguishes songguo's own refusal from the same code returned by a provider", () => {
    // Both are 429. One is the provider throttling us, the other is our own
    // rate limiter — collapsing them is the exact bug this module exists to fix.
    expect(outcomeOf(call({ status: 429, err: '' }))).toBe('vendor_error');
    expect(outcomeOf(call({ status: 429, err: ERR_SLUGS.rateLimited }))).toBe('denied_rate');

    expect(outcomeOf(call({ status: 403, err: '' }))).toBe('vendor_error');
    expect(outcomeOf(call({ status: 403, err: ERR_SLUGS.modelNotAllowed }))).toBe('denied_scope');

    expect(outcomeOf(call({ status: 404, err: '' }))).toBe('vendor_error');
    expect(outcomeOf(call({ status: 404, err: 'unmatched: POST /v1/foo' }))).toBe('unmatched');
  });

  it('splits the four things a 502 used to mean', () => {
    expect(outcomeOf(call({ status: 502, err: '' }))).toBe('vendor_error');
    expect(outcomeOf(call({ status: 502, err: 'transport_error: dial tcp: connection refused' })))
      .toBe('transport_error');
    expect(outcomeOf(call({ status: 502, err: ERR_SLUGS.noUpstream }))).toBe('no_route');
    expect(outcomeOf(call({ status: 502, err: ERR_SLUGS.requestBuildFailed }))).toBe('build_failed');
  });

  it('keeps a truncated stream distinct from a clean 200', () => {
    expect(outcomeOf(call({ status: 200, err: '' }))).toBe('ok');
    expect(outcomeOf(call({ status: 200, err: 'stream_error: unexpected EOF' }))).toBe('truncated');
  });

  it('does not blame the provider when the caller hung up', () => {
    expect(outcomeOf(call({ status: 499, err: ERR_SLUGS.clientGone }))).toBe('client_gone');
    expect(isProviderFailure('client_gone')).toBe(false);
  });

  it('keeps the legacy status-0 encoding a failure', () => {
    // status 0 with no slug was the old "no upstream response". Reading it as ok
    // would silently turn every historical transport failure into a success.
    expect(outcomeOf(call({ status: 0, err: '' }))).toBe('transport_error');
    expect(isProviderFailure(outcomeOf(call({ status: 0, err: '' })))).toBe(true);
  });

  it('refuses to guess which failure the legacy upstream_error slug was', () => {
    // It covered transport AND request-build failure. Picking one would invent a
    // fact, so it gets its own value and is charged to nobody.
    expect(outcomeOf(call({ status: 502, err: ERR_SLUGS.upstreamError }))).toBe('upstream_failed');
    expect(blameFor('upstream_failed')).toBe('none');
  });

  it('is total over every slug the gateway writes', () => {
    // The drift guard: adding a slug in Go without teaching this module fails here
    // rather than silently rendering "Unknown" in production.
    for (const slug of Object.values(ERR_SLUGS)) {
      const err = slug.endsWith(':') ? `${slug} detail` : slug;
      expect(outcomeOf(call({ status: 502, err }))).not.toBe('unknown');
    }
  });

  it('degrades an unrecognized slug visibly rather than filing it as an error', () => {
    expect(outcomeOf(call({ status: 502, err: 'something_new' }))).toBe('unknown');
  });
});

describe('blame', () => {
  it('does not charge the provider for what songguo or the caller did', () => {
    expect(blameFor('vendor_error')).toBe('provider');
    expect(blameFor('transport_error')).toBe('provider');
    expect(blameFor('truncated')).toBe('provider');

    expect(blameFor('denied_budget')).toBe('gateway');
    expect(blameFor('denied_rate')).toBe('gateway');
    expect(blameFor('no_route')).toBe('gateway');
    expect(blameFor('build_failed')).toBe('gateway');

    expect(blameFor('client_gone')).toBe('caller');
    expect(blameFor('in_flight')).toBe('none');
    expect(blameFor('abandoned')).toBe('none');
  });

  it('treats a red pill and a provider failure as different questions', () => {
    // no_route is red because it is bad, but it is our misconfiguration.
    expect(outcomeTone('no_route', 502)).toBe('err');
    expect(isProviderFailure('no_route')).toBe(false);
  });

  it('has no verdict on work that has not finished', () => {
    expect(isProviderFailure('in_flight')).toBe(false);
    expect(isProviderFailure('abandoned')).toBe(false);
  });
});

describe('isPolicyDenial', () => {
  it('exempts the limits the operator set, which clear on their own', () => {
    expect(isPolicyDenial('denied_budget')).toBe(true);
    expect(isPolicyDenial('denied_rate')).toBe(true);
  });

  it('is narrower than gateway blame, so our bugs stay visible as failures', () => {
    // All four are ours, and blameFor cannot tell them apart — which is the whole
    // reason isPolicyDenial is its own list rather than a blame comparison.
    for (const o of ['denied_scope', 'no_route', 'build_failed', 'unmatched'] as const) {
      expect(blameFor(o)).toBe('gateway');
      expect(isPolicyDenial(o)).toBe(false);
    }
  });

  it('claims nothing a provider or a caller did', () => {
    for (const o of ALL_OUTCOMES) {
      if (o === 'denied_budget' || o === 'denied_rate') continue;
      expect(isPolicyDenial(o)).toBe(false);
    }
  });
});

describe('outcomeTone', () => {
  it('follows the status for a provider error, since the status already draws that line', () => {
    expect(outcomeTone('vendor_error', 404)).toBe('warn');
    expect(outcomeTone('vendor_error', 503)).toBe('err');
  });

  it('never paints a gateway denial or an unfinished call as a failure', () => {
    expect(outcomeTone('denied_budget', 402)).toBe('gateway');
    expect(outcomeTone('in_flight', PENDING)).toBe('idle');
    expect(outcomeTone('abandoned', PENDING)).toBe('idle');
    expect(outcomeTone('client_gone', 499)).toBe('idle');
  });
});

const ALL_OUTCOMES: (Outcome | 'unknown')[] = [
  'in_flight', 'abandoned', 'ok', 'vendor_error', 'truncated', 'transport_error',
  'upstream_failed', 'no_route', 'build_failed', 'unmatched', 'denied_budget',
  'denied_rate', 'denied_scope', 'client_gone', 'unknown',
];

describe('no sentinel ever reaches the UI', () => {
  it('never renders -1 or a bare 0 as a label', () => {
    // The literal requirement. -1 and 0 are internal sentinels; a user must never
    // see either one, under any status the gateway can record.
    for (const o of ALL_OUTCOMES) {
      for (const status of [PENDING, 0]) {
        const label = outcomeLabel(o, status);
        expect(label).not.toBe('-1');
        expect(label).not.toBe('0');
        expect(label.length).toBeGreaterThan(0);
      }
    }
  });

  it('labels an in-flight and a never-finished call in words, not codes', () => {
    expect(outcomeLabel('in_flight', PENDING)).toBe('In flight');
    expect(outcomeLabel('abandoned', PENDING)).toBe('Never finished');
  });

  it('does not claim a cause it cannot know', () => {
    // A crash, a clean SIGTERM and a docker stop mid-call leave an identical row.
    expect(outcomeStyle('abandoned').label.toLowerCase()).not.toContain('crash');
    expect(outcomeStyle('abandoned').hint.toLowerCase()).not.toContain('crash');
  });

  it('shows the real code when the code IS the answer', () => {
    expect(outcomeLabel('ok', 200)).toBe('200');
    expect(outcomeLabel('vendor_error', 429)).toBe('429');
  });

  it('gives every outcome a label and an explanation', () => {
    for (const o of ALL_OUTCOMES) {
      expect(outcomeStyle(o).hint.length).toBeGreaterThan(0);
    }
  });
});
