# Songguo — Insights

> The actual insights we surface: the questions we answer with the gateway's
> data, and how each is presented. This is the *content*; the *machinery* that
> produces it is in `arch-insights.md`, and the boundary it lives behind is in
> `arch.md`.

> **Status: placeholder.** The insights layer's architecture is settled (see
> `arch-insights.md`), but the catalogue of insights — what each chart and card
> is for, what decision it supports, how it is computed and read — is still to be
> written. Add sections here as insights are defined.

## To fill in

Candidate sections, mapped to what the overview already renders:

- **Usage** — requests / spend / tokens over time; top models; spend by model;
  by vendor.
- **Tokens** — token volume; tokens by model; cache-hit ratio.
- **Context distribution** — where the input context window goes (composition
  sunburst).
- **Reliability** — success rate over time; errors by class; error rate by
  vendor.
- **Modality mix** — breakdown by modality and by user.
- **Behavioral** — session count, turns/session, duration/session,
  tokens/session, tools/session; the recent-activity feed.

For each, once written: the question it answers, the source table (`calls` vs
`sessions`), the exact aggregation, and the read endpoint.

## Performance — how fast is a provider, on OpenRouter's definition

**The question.** *Which provider answers fastest?* — split, as every model
marketplace splits it, into two independent quantities that trade off against
each other:

| | | source columns |
|---|---|---|
| **Latency (TTFT)** | how long the caller waits for the **first** token | `ttft_ms` |
| **Throughput** | output tokens per second **after** generation starts — TTFT excluded | `output_tokens * 1000 / generation_ms` |

Splitting them is the point. A relay can have excellent throughput and dreadful
TTFT (it sat on the request, then dumped the answer), or the reverse. Rolling
both into one end-to-end latency hides which half is bad, and end-to-end latency
also scales with how long the answer is — so it compares prompts, not providers.

songguo's per-call split is taken at the first real content delta of the stream
(`proxy.go`, and each wire's scanner), and the clock starts *after* the
concurrency queue, so a request that waited for a slot is not charged that wait.

**Source.** `calls`, columns `ts`, `ttft_ms`, `generation_ms`, `output_tokens`,
plus the breakdown column (`model` / `vendor` / `user_id` / client). A row
contributes a TTFT sample when `ttft_ms > 0` and a throughput sample when
`generation_ms > 0 AND output_tokens > 0`; a non-streamed call, a refusal, or a
failure that produced no output contributes to neither.

**Aggregation: the median, never the mean.** Both figures are reported as the
**p50 over the bucket's calls** — OpenRouter's default statistic, and the one
`OverviewStats` already used for its TTFT / throughput cards.

The mean is not a stylistic alternative here; it is wrong for this quantity, and
the panel said so out loud. Throughput is a **ratio with a truncated millisecond
denominator**. When a buffering relay holds the whole answer and flushes the SSE
in one go — which is exactly what songguo's Anthropic chain does, through sub2api
and then a local clash proxy — TTFT absorbs the entire call and `generation_ms`
collapses toward the floor. `Milliseconds()` truncates, so a 2,000-token answer
can land on `generation_ms = 1` and score **2,000,000 tok/s**. A single such row
dragged a whole bucket's mean: the charts showed `claude-opus-5` at 9,547 tok/s
and TTFT peaking near 80,000 ms, beside the one plausible figure on the panel.
The TTFT spike and the throughput spike were the same event, seen from the two
sides of the split. A median is unmoved by it.

Two consequences of choosing a median:

- **It cannot be folded from a sum,** so the store fetches raw per-call samples
  rather than `SUM`/`COUNT` pairs. The `WHERE` clause carries the sample guards,
  so only contributing rows leave SQLite.
- **The "Other" group pools its members' calls,** not their medians. A median of
  medians answers a different question and is not the one on the axis.

**Empty buckets are absent, not zero.** A key with no streamed call in a bucket
is omitted from the response rather than sent as `0` — a 0 would be drawn as a
measured 0 ms / 0 tok/s. The client renders the gap with `connectNulls`, so an
idle hour bridges between real measurements.

**Read endpoints.**

| what | endpoint | fields |
|---|---|---|
| per-key series, broken down | `GET /api/usage/tokens-by-model` | `points[].ttft`, `points[].tps` (both sparse) |
| overall series, no breakdown | `GET /api/usage/series` | `ttft_ms_p50`, `output_tokens_per_second_p50` |
| window summary cards | `GET /api/stats` | `ttft_ms.{p50,p95,p99}`, `output_tps.{p50,p95,p99}` |

> History: the two series endpoints reported **unweighted means of per-call
> ratios** until 2026-09-11, while `/api/stats` on the same page reported p50 —
> two definitions of one metric, disagreeing by orders of magnitude. Aligning
> them renamed `avg_ttft_ms` / `avg_output_tokens_per_second` rather than
> redefining them in place, on the same reasoning `CLAUDE.md` records for the
> `sqlFailed` split: a silently redefined field changes meaning by *not* being
> edited, and nothing forces a call site to be re-decided.
