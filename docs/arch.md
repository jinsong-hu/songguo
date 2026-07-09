# Songguo — Architecture

> The overall shape. Two companion docs go deeper: `arch-gateway.md` (the
> forwarding path) and `arch-insights.md` (the async analysis fork). The actual
> insight content — the questions we answer with the data — lives in
> `insights.md` (to be filled in). Read this first; it defines the boundary the
> other two live on either side of.

## Two concerns, one boundary

Songguo is two things wearing one binary:

1. **The gateway** — a transparent credential-swapping proxy. It authenticates
   the caller, routes to a vendor, forwards the request and response **byte for
   byte**, and records a minimal durable trace of what happened. This is the
   product's reason to exist; everything about it is latency- and
   correctness-critical.

2. **Insights** — the analysis layer. It reads the gateway's trace and derives
   everything higher-level: per-session rollups, context-window composition,
   protocol-neutral parses, the overview charts. None of it is on the request
   path.

**The boundary between them is a hard architectural line, and it is directional:
insights depends on the gateway; the gateway never depends on insights.** The
gateway writes a durable record and returns; insights forks off that record and
processes it asynchronously. If insights is slow, backed up, or crashed,
forwarding is completely unaffected — a caller cannot tell whether insights is
running at all.

This ordering is a priority statement, not just a dependency fact. **Gateway
work is never delayed or blocked by insight work, not even a little.** When the
two would contend — for a millisecond on the hot path, for a design decision,
for a schema choice — the gateway wins and insights adapts. Insights is
best-effort by construction: its failures are logged and swallowed, never
surfaced to the caller, never allowed to fail a forward.

```
                    ┌─────────────────────────────────────────┐
   client ─────────▶│  GATEWAY  (hot path, byte-transparent)   │─────────▶ vendor
   client ◀─────────│  auth → route → forward verbatim → trace │◀───────── vendor
                    └───────────────────┬─────────────────────┘
                                        │  finalized-call record
                                        │  (async fork — fire and forget)
                                        ▼
                    ┌─────────────────────────────────────────┐
                    │  INSIGHTS  (off the hot path, best-effort)│
                    │  sessions rollup · composition · parse    │
                    │  → overview charts, session views         │
                    └─────────────────────────────────────────┘
```

## The three tables

The data model mirrors the boundary. Every call touches up to three tables, and
they differ by **grain** and by **who owns them**:

| Table | Grain | Owner | Holds | Retention |
|-------|-------|-------|-------|-----------|
| **`raw`** | per-call | gateway | full raw request + response bodies and headers, capture-gated | **7 days** |
| **`calls`** | per-call | gateway | the call-level **stats** — timestamps, status, err, model/vendor/wire, normalized tokens, cost, latency, session id | **90 days** |
| **`sessions`** | per-session | insights | a **materialized rollup** of a coding-agent session — turns, tokens, duration, inferred outcome | **90 days** |

Read this as three concentric lifetimes:

- **`raw`** is the fullest and most expensive record (whole payloads) and the
  shortest-lived. It exists to debug and to feed the parse pipeline. Off by
  default (the `capture` toggle); pruned at 7 days.
- **`calls`** is the durable ledger — one row per call, enough to meter, price,
  and chart, but **not** the bodies. This is what "the stats, at call level"
  means. Pruned at 90 days.
- **`sessions`** is a **cache in semantics** — every field is derivable from the
  session's `calls` — but a durable record physically. It is written through
  incrementally as calls finalize and is **never recomputed** from `calls`, even
  while those calls still exist (see `arch-insights.md`). Pruned at 90 days on
  its own clock (by last activity).

### Why `raw` and `calls` are separate

They have different lifetimes (7d vs 90d), different sizes (whole bodies vs a
row of scalars), and different gates (`raw` only exists when capture is on;
`calls` is always written). Folding bodies into `calls` would forfeit all three
distinctions. `raw` is a 1:1 child of `calls`, keyed by the call id, `ON DELETE
CASCADE` — so pruning a call drops its body, and the 7-day body prune runs
independently and earlier.

### Why `sessions` is separate from an on-the-fly `GROUP BY`

Session rollups used to be computed at read time by grouping `calls` on
`session_id`. That ties the answer to the presence of the underlying calls and
recomputes the same aggregation on every dashboard load. Materializing it into
`sessions` (a) lets a session's summary outlive individual call detail within
the 90-day window, (b) makes the read a single indexed lookup, and (c) moves the
aggregation off the read path onto the insights fork, where a slow rollup costs
nobody anything. The trade is that `sessions` is a cache we maintain by hand —
incrementally, write-through, never rebuilt.

## Call id: a UUID minted at request-start

The call id is a **UUID string**, minted by the gateway **when the request
starts** — before the vendor is even dialed. This is load-bearing for the
two-phase write below: the gateway needs an id to reference the row it is about
to create and later update, and it needs that id before it has a response. A
database-assigned autoincrement can't be known until insert; a client-minted
UUID can be known up front. The id is a string end to end — store, API JSON,
frontend routing.

## The two-phase call lifecycle

Because incomplete calls must be recorded, a `calls` row has **two write
phases**, both owned by the gateway:

1. **Create-at-start.** When the request begins, the gateway inserts a `calls`
   row with the minted UUID, the timestamp, and the known identity (user, and
   as much of model/vendor/session as routing has resolved). Status is
   *pending*; there is no end time yet.
2. **Update-at-end.** When the response finishes (or the call fails, or the
   client aborts), the gateway updates the same row with the final status, error,
   usage, tokens, cost, latency, and end time.

A row that never reaches phase 2 — a crash, a hang, an aborted stream — stays
visible as an in-flight/interrupted call rather than vanishing. This is the
observability win of the split: the ledger shows attempts, not just
completions.

Only after phase 2 does the gateway hand the finalized record to insights.

## What lives where

- **Gateway** (`internal/proxy`, hot path): auth, scope/budget/rate gating,
  routing, verbatim forwarding, UUID minting, the two-phase `calls` write, `raw`
  capture. See `arch-gateway.md`. The settled invariants — byte-transparency,
  header-only credential rewrite, one attempt with no failover, endpoint-first
  routing — are all gateway properties and are documented in `CLAUDE.md`;
  `arch-gateway.md` consolidates them.
- **Insights** (async fork off finalized calls): the incremental `sessions`
  rollup, context-window composition, the protocol-neutral parse pipeline, and
  the read models behind the overview. See `arch-insights.md`.

## Retention

A background janitor prunes on a fixed clock, entirely on the insights/analysis
side (it is derived-data housekeeping, not forwarding). It never blocks the
gateway. Windows:

- `raw` — 7 days (by capture time)
- `calls` — 90 days (by call timestamp; cascades to `raw` and other per-call
  children)
- `sessions` — 90 days (by last activity)

There is no `VACUUM`: deleted pages are reused by new inserts, and a periodic
full vacuum would lock the database and churn disk for a steady-state gateway.
The file does not shrink; it plateaus.

## Key docs

| File | Purpose |
|------|---------|
| `arch.md` | This doc — the overall gateway/insights split and the data model |
| `arch-gateway.md` | The forwarding path (first priority; never blocked by insights) |
| `arch-insights.md` | The async analysis fork (best-effort; never blocks the gateway) |
| `insights.md` | The actual insights we surface (content — to be written) |
| `registry.md` | Wire catalogue — supported endpoints/protocols and usage extraction |
| `prd.md` | Product requirements |
| `admin-api.md` | Admin/config API |
