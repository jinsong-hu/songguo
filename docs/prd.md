# Songguo — Transparent AI Usage Gateway

> Bring transparency to AI usage.

A self-hosted, single-tenant gateway that sits in front of every AI provider you use — and does exactly three things: **auth, billing, observability**. It never rewrites your traffic. One binary, your keys, your budgets, your data.

---

## 1. Problem

An individual developer or small team building an AI app today calls **many providers across many modalities** — chat, embeddings, image, video, speech (TTS/STT/realtime), and increasingly **MCP tools** (search, image-gen). Often through **several resellers/号池** at once (supplier A sells opus+gpt-4o, supplier B sells gpt-4o+deepseek).

They need one boring thing: **a place to manage keys + budgets, see real spend, and not wake up to a runaway bill.** The existing options all force a bad trade:

| Tool | Built for | Why it's wrong for self-use |
|------|-----------|------------------------------|
| **New API / one-api** | Multi-**tenant** public reseller 中转站 | Drags a whole operator layer — user accounts, registration, 充值/支付, 分销, 兑换码, 运营后台. Massive overkill, maintenance, and attack surface when you're the only user. |
| **LiteLLM** | Single-owner virtual keys + budgets | Good budgets, but it **normalizes everything to OpenAI format** → weak on China async-task modalities (image/video/Suno/可灵) and private protocols; Python; config-heavy. |
| **Higress** | Infra-grade AI+API gateway | Envoy/Istio/Wasm — needs a platform team. Exposes the infrastructure instead of hiding it. |

**Whitespace:** nobody offers a dead-simple, single-tenant, self-hosted gateway that does auth + budget + observability across **every modality** with **near-zero maintenance**.

The distinction that matters: **multi-tenant** (independent user accounts, payments, reseller) ≠ **multi-token** (one owner, many keys each with a budget + scope). Songguo is the latter, minus all the SaaS-operator baggage.

---

## 2. Core principle — transparent, not translating

**A gateway shouldn't *translate*. It should be *transparent*.**

Songguo **never rewrites a request or response**, never converts async→sync, never normalizes to a unified format. In the middle it does only:

1. **Auth** — swap the consumer's Songguo token for the real upstream credential (the only thing it mutates: the credential, not the body).
2. **Billing / metering** — read-only.
3. **Observability** — read-only.

It is a **gate + meter, not a transformer**. It may *reject* an over-budget call; it never *transforms* a forwarded payload.

This single decision is the whole product:

- **Kills the maintenance hell.** New API / LiteLLM break every time a vendor adds a field or ships a model, because their per-vendor request/response translation has to be updated and always lags. No-rewrite = **new vendor features work day one, near-zero maintenance.** This is the wedge.
- **Widest possible coverage.** Because it doesn't try to understand the protocol, it can proxy things the translators can't — e.g. 豆包/火山 realtime ASR over a private binary WebSocket: forward the frames, inject auth, meter by duration/bytes. No translation required.
- **Async is free.** The gateway does not own the submit→poll lifecycle. The consumer owns it and points each call at the gateway; the gateway just forwards + meters each one. No task state machine.

### Honest consequences (acceptable / by design)
- **No unified client format.** Consumers use each vendor's **native SDK** pointed at the Songguo base URL. Right for a small team that already picked its vendors and just wants centralized auth+budget+stats.
- **Metering = read-only sniffing.** Parse the native payload to extract usage (tokens / images / seconds / chars / calls); if a shape isn't recognized, **fall back to coarse metering** (bytes / duration / call-count). **Parsing failure must never block traffic.**
- **A per-modality pricing table must be maintained** — inherent to *any* cost-tracking gateway, not extra.

---

## 3. Positioning

**Single-tenant, multi-token. For individual developers and small businesses running their own gateway. 极简, low-maintenance.**

One binary. No accounts, no payment rails, no service mesh. You run it, you point your apps at it, you stop worrying about spend.

---

## 4. Core model — only three concepts

Everything is **Channel**, **Token**, **Ledger**. Complexity (async polling, MCP transports, private protocols) hides behind the Channel; it never becomes a concept the user has to hold.

### 4.1 Channel — an upstream, with a 号池
A model channel **or** an MCP channel.

| Field | Meaning |
|-------|---------|
| `base_url` | Where requests are forwarded |
| `served_models[]` | Which models this channel can serve (used to auto-derive routing) |
| `credentials[]` | The **号池** — one or more keys, rotated to spread per-key rate limits |
| `weight` / `priority` | Routing policy inputs |
| `per_model_price` | For true-cost metering + cheapest-route |
| `health` | Read from upstream responses (429/5xx) for observability. No auto bring-down today — a failing vendor is not demoted automatically (future, server-side) |

### 4.2 Token — a scoped budget
| Field | Meaning |
|-------|---------|
| `budget` | Hard cap (enforced — over-budget calls are rejected, not transformed) |
| `scope` | Which models **and tools** this token may use |
| `rate_limit` | Per-token RPM |

### 4.3 Ledger — one row per call
Pure read-only metering output — chat / image / video / TTS / STT / embedding / **MCP-tool** all land here as rows.

| Field | Meaning |
|-------|---------|
| `serving_channel`, `credential_id` | Which channel/key served it (one attempt per call — no failover, so no attempt counter) |
| `model` / `modality` | What was called |
| `usage`, `cost` | Supports **per-$ and per-call** metering (tools are often per-call/free) |
| `latency`, `status` | Observability |
| `tags` (optional) | Business attribution — see §6 |

---

## 5. 号池 — routing (a policy on Channel, not a new concept)

For user-facing traffic you need a pool. It folds into Channel and **does not break the transparent principle**: routing = *pick destination + inject that upstream's credential + swap base_url*; the body is untouched.

Two senses of 号池, both supported:
1. **Multi-credential within one Channel** — rotate keys to dodge per-key rate limits.
2. **Multi-Channel per model** — e.g. A=[opus, gpt-4o], B=[gpt-4o, deepseek]. A request for `gpt-4o` has candidates {A, B} → load-balance across them.

The gateway **auto-derives** "which channels can serve model X" from each channel's `served_models`, then picks by policy (priority → weighted round-robin). It forwards to **one** candidate and surfaces the result verbatim — **no per-call failover**. A vendor that returns 429/5xx (or errors on the transport) is not auto-demoted today: the failing request sees the real error, the client decides whether to retry, and the vendor stays selected until an operator changes config. (Auto bring-down of a persistently-failing vendor is a future, server-side feature.) **No** New-API-style model-group / 模型重定向 / 倍率分组 config subsystem.

```
Channel A: models=[opus, gpt-4o],     creds=[…], price=…
Channel B: models=[gpt-4o, deepseek], creds=[…], price=…

opus     → {A}      → A
deepseek → {B}      → B
gpt-4o   → {A, B}   → policy pick (A on top by priority/weight)
```

Falls out for free: per-channel price → **route-cheapest**, and the Ledger records the serving channel/credential, so cost is true even when A's and B's markups differ.

> Superseded: earlier drafts had the proxy fail a request over to the next
> candidate on 429/5xx mid-call, and the router auto-demoted a failing vendor
> into a health cooldown. Both were dropped (2026-07-03) as inconsistent with
> byte/behavior transparency — we surface the failure and let the client decide,
> and we do not auto bring-down vendors today (a future server-side feature). So
> the streaming-billing edge (bill per-attempt on mid-stream failover) no longer
> arises.

---

## 6. MCP proxying (in scope)

Small teams building AI apps commonly need MCP tools (search, image-gen). MCP folds into the same model with **no new core concept**:

- An MCP server = another **Channel**.
- A tool call = another **Ledger** row.
- One **Token** budget covers **models + tools**.

The pitch: **one budget / one key / one observability plane over everything the app calls — models *and* tools.** (Cleaner than Higress, which treats them as separate subsystems.)

Caveat: many tools are self-hosted / free / per-call, so Ledger cost + budgets support **per-call counting**, not only dollars.

---

## 7. Analytics — business-oriented, not reseller- or infra-oriented

New API's stats answer "who spent what / how much do I earn" (reseller). LiteLLM's answer "spend/latency/error per key" (infra). Songguo answers **product/business** questions. The unlock is changing the **unit of measurement** from per-request/per-key to **business units**.

| Category | What it shows | Why a small team cares |
|----------|---------------|------------------------|
| **Unified cost** | One $ across all modalities (token/image/sec/char/min normalized) + per-modality split | The standout — it proxies all modalities |
| **Attribution** | Cost per **feature / end-customer / session / outcome** via optional tags | Unit economics, pricing |
| **Forecast** | Burn rate + **budget runway** ("~19 days left at this pace") | Not "how much spent" but "how long until trouble" |
| **Reliability** | p50/p95/p99 latency, fallback/retry/cache-hit | Their app's UX |
| **Guardrails** | **Proactive spend-spike / anomaly / budget-burn alerts** | Small teams can't watch dashboards — they need to be *told* |
| **Optimization** | Cross-model cost×latency comparison | Save money, pick cheaper models |

**The one tradeoff to design around:** zero-instrument (model/modality/tokens/latency/status — free but generic) vs **tag-based** (feature/customer/session/outcome — needs the app to pass an optional, OpenAI-compatible `metadata` field/header). **Default-useful with zero tags; auto light-up richer views when tags are present.**

---

## 8. What Songguo explicitly does NOT do

- No user accounts, registration, login
- No payment rails / 充值 / top-up
- No 分销 / reseller / 兑换码
- No request/response translation / unified client format
- No model-group / 重定向 / 倍率分组 config subsystem
- No service mesh / k8s requirement

Removing these *is* the product.

---

## 9. Relationship to jinsong — the umbrella

Songguo and **jinsong** (`jinsong.hu`, the agent-session experience-analytics product) ladder up to one mission: **"bring transparency to AI usage."** Two layers:

1. **Usage / cost transparency** — *Songguo*. In-path, frictionless front door (point your base_url at it). Broad audience: anyone spending on AI APIs.
2. **Experience / quality transparency** — *jinsong*. Deeper instrumentation, narrower, holds the benchmark moat.

Strategic logic: Songguo is the **wide, low-friction front door**; jinsong is the **deep, defensible engine**. Songguo also solves jinsong's hardest problem — *getting telemetry at all* — by being a zero-effort usage-data source and funnel.

**Boundary (don't oversell):** a pure proxy can never see experience metrics (stalls, TTFR, completion, AXS need agent-level events). Songguo lights up the **usage/cost** layer only.

Keep them **two composable products, not merged.**

---

## 10. UI direction

Modern **dev-tool / app-shell** aesthetic — sidebar nav + toolbar, crisp grotesk, dense data tables, one restrained accent, mono only for IDs/numbers. **Not** editorial/serif/broadsheet. Reference lane: Linear / Vercel / Railway. Light and dark both mocked (see `samples/` if added). Home = Overview (unified spend, runway, anomaly, live ledger, channels/号池, tokens).

---

## 11. Architecture (shared philosophy with jinsong)

One binary, single-tenant, **SQLite default**, self-hosted, near-zero ops. Auto-migrate on start. The Ledger is the spine — an append-only table of sniffed calls; every view is a query over it.

> **Open architectural fork (decide before building):** is Songguo **(a)** a standalone product that merely shares the brand, or **(b)** a deliberate **telemetry front-end for jinsong** — emitting jinsong's event schema from day one (= one platform)? (b) is a real commitment that shapes the Ledger and event model.

---

## 12. MVP scope

**v1 (sync first):**
- Channel / Token / Ledger
- 号池 routing (priority → weighted RR, credential rotation; one attempt, no per-call failover, no auto bring-down)
- Budget enforcement (reject over-budget)
- Sync modalities: chat, embedding, sync TTS/STT
- Read-only metering with coarse fallback
- Overview dashboard + spend ledger + per-token budgets + anomaly alert

**v2:**
- Async multimodal channels (image/video — consumer owns the poll loop, gateway forwards + meters)
- MCP channels (tool metering, per-call budgets)
- Tag-based business attribution
- Burn-rate forecast + cross-model comparison

---

## 13. Open questions

1. **Standalone vs telemetry front-end for jinsong** (the §11 fork) — decides the event/Ledger model.
2. Naming & brand under `jinsong.hu`.
3. Pricing-table maintenance: ship curated price packs per vendor, or user-supplied?
4. How far to push read-only sniffing for streaming (SSE/WS) without buffering/latency?
5. Default theme: light or dark.
