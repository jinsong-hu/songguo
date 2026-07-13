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
- **Performance** — end-to-end latency, streamed TTFT, and output tokens/sec.
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
