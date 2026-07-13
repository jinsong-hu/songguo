# Songguo — Insights Architecture

> The analysis layer. Everything here is **downstream of the gateway and off the
> hot path**. Companion to `arch.md` (the overall split) and `arch-gateway.md`
> (the forwarding path upstream of this doc). The actual insight content — the
> questions we answer — lives in `insights.md` (to be written); this doc is the
> machinery.

## The contract with the gateway

Insights consumes one thing: the **finalized-call record** the gateway hands off
after phase 2 of the two-phase write (see `arch-gateway.md`). From that record
it derives everything higher-level. It gives the gateway back nothing.

The contract is deliberately one-way and best-effort:

- **Insights never blocks the gateway.** The hand-off is a fork — fire and
  forget. The gateway does not wait on it, does not check whether it succeeded,
  does not retry it.
- **Insights failures are invisible.** Every insights step logs its own errors
  and swallows them. A failed rollup, a parse error, a full work queue — none of
  it ever surfaces to the caller or fails a forward. A dropped insight is a
  missing data point, not an incident.
- **Insights forks the data.** It reads from the gateway's `calls`/`raw` record;
  it is not in the serving path and cannot slow it. If insights is down, the
  gateway keeps forwarding and recording; insights simply falls behind or gaps,
  and catches up (or doesn't) without anyone waiting.

This mirrors the async patterns the codebase already uses off the hot path — the
protocol parse pipeline and context composition both run after the client
response is sent and log-and-drop on failure. Insights generalizes that into a
named layer.

## What insights owns

1. **The `sessions` rollup** — a materialized, incrementally-maintained summary
   of each coding-agent session. The centerpiece; detailed below.
2. **Context-window composition** — a local (o200k_base) token decomposition of
   a chat request's input context across sources (system, tool schemas, tool
   results, …), decoupled from official billing usage. Read-only sniff of the
   buffered request body, off the hot path.
3. **The protocol-neutral parse pipeline** — turns captured `raw` bodies into a
   structured, vendor-independent `parse.Call`. Runs only when capture is on
   (there are bodies to parse). Async worker pool.
4. **The read models behind the overview** — the aggregate queries that power the
   dashboard. Overview charts stay **call-grained** (they read `calls`, so all
   traffic appears, session-bearing or not); session-specific cards read
   `sessions`.

## The `sessions` cache: write-through, never recomputed

`sessions` is the load-bearing design decision of the insights layer, so its
rules are explicit.

**It is a cache in semantics, a durable record physically.** Every field in a
`sessions` row is derivable from that session's `calls`. But we do not treat it
as a view to be recomputed on demand — we treat it as a record we maintain by
hand.

**It is updated incrementally, write-through, as each call finalizes.** When the
gateway hands off a finalized call that carries a non-empty `session_id`,
insights folds that one call into the session's row: bump the turn count, add its
tokens/cost, extend the time bounds, update the last-status (which drives the
inferred outcome), note a subagent if the call carried a parent-agent id. One
call in, one row updated (or created on the session's first call).

**It is never recomputed from `calls`.** Not on read, not on a timer, not even
when all the underlying calls are still present. This is a firm rule, not an
optimization we might relax:

- The rollup is **the** record of the session's stats. It is authoritative on its
  own terms.
- Because it is never rebuilt, it can outlive individual call detail. Within the
  90-day window the two prune on independent clocks; a session summary does not
  depend on its calls still existing.
- Recomputation would reintroduce exactly the read-time `GROUP BY session_id`
  cost the materialization exists to remove, and would make the answer flicker
  as calls age out. We accept the flip side: if a call's contribution is ever
  missed (insights was down at that instant), the rollup is permanently slightly
  off for that session. That is the accepted price of never rebuilding — a
  best-effort cache, consistent with the whole layer being best-effort.

### What a `sessions` row holds

Enough to answer the session-level questions without touching `calls`:

- identity: `session_id`
- time bounds: first activity, last activity (last activity is the prune key and
  the feed ordering key)
- counts: turns (calls), error count
- sums: input/output tokens, cost
- outcome signal: the status of the newest call seen, from which
  completed / errored / interrupted is inferred at read time
- structure: whether any call carried a parent-agent id (had subagents)

Session-less traffic (ordinary API calls with an empty `session_id`) produces
**no** `sessions` row — it lives only in `calls`. That is why the overview's
call-grained charts read `calls`: so that traffic is not invisible. Only real
coding-agent sessions get a `sessions` row.

### Inferred outcome (unchanged semantics)

Outcome is read off the session's last call by time and is an interaction-level
signal, not a judgment about the underlying task (the gateway never sees that):

- **Interrupted** — the last call had no upstream response (status 0): the client
  aborted mid-stream.
- **Errored** — the last call returned 4xx/5xx.
- **Completed** — the last call returned 2xx/3xx.

With the two-phase write, a session whose last call is still *pending* (created
but not finalized) reads as in-flight until that call finalizes or is pruned.

## Read paths

- **Overview aggregate charts** (requests/spend/tokens over time, breakdowns by
  model/vendor/user/modality, error classes, latency percentiles) — read
  `calls`, call-grained, windowed. All traffic appears.
- **Behavioral cards** (session count, turns/session, duration/session,
  tokens/session, tools/session) — read the materialized `sessions` table. The
  section shows agent-run behavior, not outcome; the inferred outcome the
  rollup still stores drives the activity feed, not these cards.
- **Recent activity feed** — blends both: coding-agent sessions surface as
  session rows (from `sessions`), standalone calls surface as request rows (from
  `calls`). Ordered by last activity.
- **Per-call detail / trace** — reads `calls` for the row and `raw` for the
  bodies (when captured and not yet pruned).

## Retention (analysis-side housekeeping)

A background janitor, on the insights side, prunes on a fixed clock. It is
derived-data maintenance and never blocks the gateway:

- `raw` — 7 days (by capture time)
- `calls` — 90 days (by call timestamp; cascades to per-call children)
- `sessions` — 90 days (by last activity)

Because `sessions` is never recomputed, pruning it is final: once a session's
last activity ages past 90 days, its row is deleted and not rebuilt. No
`VACUUM` — freed pages are reused; the database plateaus rather than shrinks.
