// The scaffolding every dashboard page shares: the time range, the three
// top-bar filters, and the fetches more than one section reads.
//
// Sections own their own series fetch and their own breakdown selector; this
// owns what sits above them. A page is then just "which sections, in what order,
// defaulting to which breakdown".

import { useMemo, useRef, useState } from 'react';
import { api } from '../../api/client';
import type { UsageDimension, UsageFilter } from '../../api/types';
import { FacetSelect } from '../FacetSelect';
import { TimeRangePicker } from '../TimeRangePicker';
import { LIVE_REFRESH_MS, useFetch, useLiveTick } from '../../lib/useFetch';
import { BrandIcon, ModelIcon, modelDisplayName, providerBrand } from '../../lib/modelBrand';
import { clientIconSvg, clientLabel } from '../../lib/client';
import {
  DEFAULT_RANGE,
  type TimeRange,
  deriveBucket,
  isRolling,
  rangeLabel,
  resolveRange,
} from '../../lib/timeRange';
import { dims as pickDims, type SeriesScope } from './Section';

const REFRESH_MS = LIVE_REFRESH_MS;

export interface DashboardOptions {
  /** Breakdown dimensions this page offers, in the order they appear. */
  dimensions: UsageDimension[];
  /**
   * A consumer key sees only its own traffic, so the by-user breakdown is
   * meaningless and the users roster is not fetchable. Drops both.
   */
  isUser: boolean;
}

export function useDashboard({ dimensions, isUser }: DashboardOptions) {
  const [range, setRange] = useState<TimeRange>(DEFAULT_RANGE);
  // Top-bar filters. Empty = all, so the default costs no query params. These
  // narrow *which calls* every panel is computed from; the per-section
  // segmented controls below choose how to *split* them. The two compose.
  const [models, setModels] = useState<string[]>([]);
  const [vendors, setVendors] = useState<string[]>([]);
  const [clients, setClients] = useState<string[]>([]);
  const tick = useLiveTick(REFRESH_MS);

  // The rolling/absolute fork lives here. A rolling range re-resolves on every
  // tick so "Last 24 hours" keeps sliding; an absolute one must not, so the tick
  // is deliberately excluded from its dependencies — otherwise a pinned window
  // would creep forward on each refresh. `rollingTick` is the whole mechanism:
  // it is the live clock for rolling ranges and a constant for absolute ones.
  //
  // resolveRange can return null (an expression the user is still typing), so
  // fall back to the last good window rather than tearing the charts down.
  // The +1 is load-bearing: the store filters `ts < until`, so resolving `now`
  // to the current second would exclude a call that landed inside it.
  const rolling = isRolling(range);
  const rollingTick = rolling ? tick + 1 : 0;
  const lastGood = useRef<{ since: number; until: number } | null>(null);

  const { since, until } = useMemo(() => {
    const resolved = resolveRange(range, rolling ? new Date(rollingTick * 1000) : undefined);
    if (resolved) {
      lastGood.current = resolved;
      return resolved;
    }
    return lastGood.current ?? { since: rollingTick - 86400, until: rollingTick };
  }, [range, rolling, rollingTick]);

  // Granularity follows the window instead of being pinned per preset, so a
  // one-hour view gets minute buckets and a 90-day view does not ask for 2160
  // hourly points. Each chart still re-reads the bucket the server actually used
  // off its own response.
  const bucket = useMemo(() => deriveBucket(since, until), [since, until]);

  const filter = useMemo<UsageFilter>(
    () => ({ models, vendors, clients }),
    [models, vendors, clients],
  );
  const filterKey = `${models.join('\x00')}|${vendors.join('\x00')}|${clients.join('\x00')}`;
  const rangeKey = JSON.stringify(range);

  const opts = { intervalMs: REFRESH_MS };
  const adminOpts = { intervalMs: REFRESH_MS, enabled: !isUser };

  // The filter option lists. Keyed only on the window — passing the current
  // selection would let choosing a model delete providers from the other list.
  const facets = useFetch(() => api.facets(since, until), [since, until], opts);
  const overview = useFetch(
    () => api.overview(since, until, filter),
    [since, until, filterKey],
    opts,
  );
  const sessions = useFetch(
    () => api.sessionsOverview(since, until, filter),
    [since, until, filterKey],
    opts,
  );
  // Resolve user-id series keys to display names for the by-user views. Admin
  // only — a user key drops the by-user dimension, so it needs no name map.
  const usersList = useFetch(() => api.users(), [], adminOpts);

  const userNames = useMemo(
    () => new Map((usersList.data ?? []).map((u) => [u.id, u.name])),
    [usersList.data],
  );

  const offered = useMemo(
    () => pickDims(...dimensions.filter((d) => !(isUser && d === 'user'))),
    [dimensions, isUser],
  );

  const scope: SeriesScope = {
    since,
    until,
    bucket,
    filter,
    filterKey,
    rangeKey,
    intervalMs: REFRESH_MS,
    dims: offered,
    userNames,
  };

  // Clients is mounted only when it has something to offer: unlike models and
  // providers it can legitimately be empty (a gateway seeing no coding-agent
  // traffic), and a select with no options is a dead control. The second half of
  // the guard keeps it mounted while a selection survives — a filter you cannot
  // see is a filter you cannot clear, the same rule FacetSelect follows for
  // options that fall out of the window.
  const showClients = (facets.data?.clients?.length ?? 0) > 0 || clients.length > 0;
  const toolbar = (
    <>
      <FacetSelect
        label="models"
        options={facets.data?.models ?? []}
        value={models}
        onChange={setModels}
        renderIcon={(k) => <ModelIcon model={k} size={15} />}
        renderLabel={modelDisplayName}
      />
      <FacetSelect
        label="providers"
        options={facets.data?.vendors ?? []}
        value={vendors}
        onChange={setVendors}
        renderIcon={(k) => {
          const brand = providerBrand(k, []);
          return brand ? <BrandIcon brand={brand} label={k} size={15} /> : null;
        }}
      />
      {showClients ? (
        <FacetSelect
          label="clients"
          options={facets.data?.clients ?? []}
          value={clients}
          onChange={setClients}
          renderIcon={(k) => {
            // Raw markup from thesvg, sized by the .icon slot's CSS. Null when we
            // have no icon, so the fixed-width slot keeps the names aligned.
            const icon = clientIconSvg(k);
            return icon ? (
              <span aria-hidden="true" dangerouslySetInnerHTML={{ __html: icon }} />
            ) : null;
          }}
          renderLabel={clientLabel}
        />
      ) : null}
      <TimeRangePicker value={range} onChange={setRange} />
    </>
  );

  return {
    range,
    scope,
    toolbar,
    facets,
    overview,
    sessions,
    /** True when any top-bar filter is active — sections word their hints on it. */
    filtered: models.length + vendors.length + clients.length > 0,
    windowLabel: rangeLabel(range),
  };
}
