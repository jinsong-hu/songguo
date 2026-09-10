// Package catalog serves the read-only directory of known providers and the
// models they serve, which the dashboard lists for one-click add. It is pure
// reference data, independent of what the operator has actually configured;
// adding a catalog provider instantiates a configured provider (in the store)
// from the preset, pre-filling everything except the API key.
//
// # Shape
//
// The types mirror models.dev's api.json exactly — a map keyed by provider id,
// each provider carrying a map of models with modalities/limit/cost. That is
// deliberate: cmd/catalogsync copies models.dev's model subtrees verbatim
// rather than translating them, so there is no field mapping to drift, no unit
// conversion, and no rounding step that could make a re-sync see a difference
// where none exists.
//
// # Two files, two owners
//
//   - models.json is generated. cmd/catalogsync owns every byte; do not hand-edit.
//   - catalog.json is hand-maintained. It holds the routing topology — endpoints,
//     adapters, quirks, the custom template — which models.dev has no notion of
//     because everything there is SDK-shaped rather than URL-shaped. It also holds
//     the models models.dev cannot supply: every Volcengine model (no first-party
//     provider exists there), Alibaba's and Zhipu's embeddings, and everything
//     billed per character, per second or per call.
//
// Load merges them, and a hand-written entry always wins — which doubles as the
// escape hatch for pinning a rate the generator would otherwise overwrite.
package catalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed models.json
var generated []byte

//go:embed catalog.json
var manual []byte

// Catalog is the provider directory, keyed by provider id.
type Catalog map[string]Provider

// Provider is one upstream's preset. The first group of fields is models.dev's
// provider shape; Endpoints, Quirks and Custom are songguo's own and have no
// models.dev counterpart.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Doc    string           `json:"doc,omitempty"`
	Env    []string         `json:"env,omitempty"`
	NPM    string           `json:"npm,omitempty"`
	Models map[string]Model `json:"models"`

	// Endpoints binds each wire to its full upstream URL and auth adapter.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Quirks parameterize usage extraction (see internal/wire) for the whole
	// provider. They never change what is sent.
	Quirks map[string]string `json:"quirks,omitempty"`
	// Custom marks a template provider with no preset models or pinned upstream:
	// the operator supplies the base URL (substituted into the {base} placeholder
	// in each endpoint) and types their own model ids. Used for the "Custom" tile.
	Custom bool `json:"custom,omitempty"`
}

// Endpoint is one preset wire bound to its full upstream URL + adapter, 1:1 with
// the wire. The URL is used as-is by the proxy and may carry a {model}
// placeholder (or, on Custom providers, a {base} placeholder). Models lists the
// model ids this endpoint serves — and is also what cmd/catalogsync reads to
// decide which models to generate; companion wires like a model-listing endpoint
// carry none.
type Endpoint struct {
	Wire     string   `json:"wire"`
	Endpoint string   `json:"endpoint"`
	Adapter  string   `json:"adapter"`
	Docs     string   `json:"docs,omitempty"`
	Note     string   `json:"note,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// Model is one model's descriptive metadata and price, in models.dev's shape,
// plus Note — songguo's own, with no models.dev counterpart.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Note is why this entry is written the way it is: a retired id still
	// accepted under another model, a rate pinned against a stale upstream, a
	// dated repricing someone has to come back for. It is rendered beside the
	// model in the provider editor rather than kept as a comment nothing reads,
	// because the operator looking at a surprising rate is exactly who needs it.
	// Only the hand-written catalogue sets it; a generated entry never does.
	Note        string     `json:"note,omitempty"`
	Family      string     `json:"family,omitempty"`
	Attachment  bool       `json:"attachment,omitempty"`
	Reasoning   bool       `json:"reasoning,omitempty"`
	ToolCall    bool       `json:"tool_call,omitempty"`
	Temperature bool       `json:"temperature,omitempty"`
	ReleaseDate string     `json:"release_date,omitempty"`
	LastUpdated string     `json:"last_updated,omitempty"`
	OpenWeights bool       `json:"open_weights,omitempty"`
	Modalities  Modalities `json:"modalities,omitempty"`
	Limit       Limit      `json:"limit,omitempty"`
	Cost        Cost       `json:"cost"`
}

// Modalities is what a model accepts and produces: text, image, audio, video,
// file (models.dev's vocabulary; the dashboard labels the first four).
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Limit is the model's context window and per-message caps. Zero means the
// upstream does not publish one.
type Limit struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Cost is the rate for each quantity songguo meters. Every field is optional and
// they are **additive**, not alternatives: a model's cost is the sum over the
// axes it declares. That is what lets an audio model bill tokens and seconds at
// once, which the old single-`unit` price could not express — it would price one
// axis and silently compute $0 for the other.
//
// Each axis maps 1:1 onto a wire.Normalized field, so adding a metered quantity
// means adding a field here and a term in pricing.Cost, and nothing else.
//
// The two groups differ in scale, and the split is not ours to remove: the first
// four are models.dev's own fields at models.dev's basis (USD per 1M tokens), so
// generated entries stay byte-identical to upstream. The rest are songguo
// extensions for wires models.dev does not cover, at the basis the vendors
// publish (USD per single unit).
type Cost struct {
	// Per 1M tokens — models.dev's fields, copied verbatim.
	Input      float64 `json:"input,omitempty" yaml:"input"`             // fresh input tokens
	Output     float64 `json:"output,omitempty" yaml:"output"`           // output tokens
	CacheRead  float64 `json:"cache_read,omitempty" yaml:"cache_read"`   // cache-read input tokens
	CacheWrite float64 `json:"cache_write,omitempty" yaml:"cache_write"` // cache-write input tokens

	// Per single unit — songguo extensions. models.dev has no unit concept and
	// no field of any kind for these, across all 5,901 models it lists.
	Character float64 `json:"character,omitempty" yaml:"character"` // per character (volc TTS)
	Second    float64 `json:"second,omitempty" yaml:"second"`       // per second of audio (volc ASR)
	Image     float64 `json:"image,omitempty" yaml:"image"`         // per image
	Call      float64 `json:"call,omitempty" yaml:"call"`           // per request (volc video)

	// Tiers raises the token rates for large requests. Vendors charge more once
	// a prompt crosses a context threshold — gpt-5.6-luna doubles above 272k —
	// and ignoring that under-bills exactly the long agent contexts songguo is
	// built to route. See Resolve.
	Tiers []CostTier `json:"tiers,omitempty" yaml:"tiers"`

	// Schedules raises the token rates during the vendor's published peak hours.
	// DeepSeek charges double on weekday business hours, which no static rate can
	// express: pick either number and you are wrong for part of every week.
	//
	// Unlike every other field here this one is songguo's alone and can only ever
	// come from the hand-written half of the catalogue. models.dev models no time
	// axis at all — across its 213 providers a `cost` object carries only input,
	// output, cache_read, cache_write, reasoning, input_audio, output_audio,
	// context_over_200k and tiers, and all 349 tiers upstream are type "context".
	// So there is nothing to sync and nothing a refresh can overwrite. See Resolve.
	Schedules []CostSchedule `json:"schedules,omitempty" yaml:"schedules"`
}

// CostTier is the token rates that apply once a request crosses Tier.Size. The
// nesting is models.dev's own shape, kept verbatim so a generated entry stays
// byte-identical to upstream rather than needing a translation step.
//
// A tier states only the axes that change; anything it omits keeps the base
// rate. Seven models upstream publish a tier that leaves out an axis their base
// declares, and reading that as "unchanged in this bracket" changes only what
// the vendor said changed.
type CostTier struct {
	Input      float64   `json:"input,omitempty" yaml:"input"`
	Output     float64   `json:"output,omitempty" yaml:"output"`
	CacheRead  float64   `json:"cache_read,omitempty" yaml:"cache_read"`
	CacheWrite float64   `json:"cache_write,omitempty" yaml:"cache_write"`
	Tier       TierBound `json:"tier" yaml:"tier"`
}

// TierBound is what a tier is measured against. Only "context" exists upstream
// today (349 of 349 tiers); an unrecognized type is ignored rather than guessed
// at, so a new kind of bracket bills at the base rate until it is implemented
// instead of silently applying a threshold we do not understand.
type TierBound struct {
	Type string `json:"type" yaml:"type"`
	Size int    `json:"size" yaml:"size"`
}

// TierContext is the only tier type upstream publishes: the bracket is chosen by
// how large the request's input is.
const TierContext = "context"

// CostSchedule is the token rates that apply while the clock is inside any of
// its windows. It mirrors CostTier deliberately — same four axes, same rule that
// an axis it omits keeps the base rate — so the two conditional forms read the
// same way and neither needs its own mental model.
//
// One rate set, several windows: a vendor that raises prices twice a day states
// one schedule with two windows rather than two schedules repeating the same
// numbers. DeepSeek's peak is 09:00-12:00 and 14:00-18:00, which is exactly this
// shape.
type CostSchedule struct {
	Input      float64  `json:"input,omitempty" yaml:"input"`
	Output     float64  `json:"output,omitempty" yaml:"output"`
	CacheRead  float64  `json:"cache_read,omitempty" yaml:"cache_read"`
	CacheWrite float64  `json:"cache_write,omitempty" yaml:"cache_write"`
	When       []Window `json:"when" yaml:"when"`
}

// Window is a recurring stretch of wall-clock time, in the vendor's own terms.
//
// # Why an offset and not a timezone name
//
// Vendors publish these rules in their local time — DeepSeek says "北京时间周一至
// 周五 9:00-12:00". Beijing has no DST, so a fixed offset reproduces that rule
// exactly while keeping the file readable as the vendor wrote it; rewriting the
// hours into UTC would be equally correct and would lose the ability to check the
// file against the vendor's page at a glance. It also means pricing never depends
// on tzdata being present in the runtime image.
//
// A vendor whose peak hours observe DST cannot be expressed this way, and must
// not be faked with an offset that is right for half the year. That is what Type
// is for: a new kind of window gets a new type and its own matcher.
type Window struct {
	// Type selects how the window is interpreted. WindowWeekly is the only one.
	Type string `json:"type" yaml:"type"`
	// Offset is the fixed UTC offset the clock times below are stated in,
	// "+08:00" style. Empty or "Z" means UTC.
	Offset string `json:"offset,omitempty" yaml:"offset"`
	// Days are the weekdays the window applies on, "mon".."sun".
	Days []string `json:"days" yaml:"days"`
	// Start and End are "HH:MM" in Offset's zone. The interval is HALF-OPEN:
	// 09:00-12:00 includes 11:59:59 and excludes 12:00:00, so two adjacent
	// windows can meet without overlapping and no instant is priced twice.
	Start string `json:"start" yaml:"start"`
	End   string `json:"end" yaml:"end"`
}

// WindowWeekly is a window that repeats on the same weekdays every week. It is
// the only shape any vendor songguo maps publishes.
const WindowWeekly = "weekly"

// weekdays maps the day names a window may use onto Go's. Lowercase three-letter
// names only: a single spelling means Validate can reject a typo outright rather
// than quietly matching nothing, which would under-bill in silence.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Validate reports what is wrong with a window, or nil.
//
// Every window in the catalogue is checked at load (see Load), and the checking
// is STRICT — an unusable window is an error, where an unrecognized TierBound
// type is merely ignored. The asymmetry is deliberate and worth stating, because
// the two look like the same situation and are not:
//
//   - A tier arrives from models.dev, which can add a bracket type any day
//     without telling us. Ignoring one we do not understand bills at the base
//     rate, which is wrong but bounded, and beats failing to boot over an
//     upstream edit nobody here made.
//   - A window can only come from catalog.json, which is ours. There is no
//     upstream that can surprise us, so an unusable window is a typo in a file we
//     wrote — and the failure mode of ignoring it is billing every peak hour at
//     the off-peak rate, silently, forever. Refusing to load is strictly better
//     than that, and it surfaces at build and test time rather than to an
//     operator.
func (w Window) Validate() error {
	if w.Type != WindowWeekly {
		return fmt.Errorf("unknown window type %q (only %q exists)", w.Type, WindowWeekly)
	}
	if _, ok := w.location(); !ok {
		return fmt.Errorf("offset %q is not a ±HH:MM UTC offset", w.Offset)
	}
	if len(w.Days) == 0 {
		return errors.New("window names no days")
	}
	for _, d := range w.Days {
		if _, ok := weekdays[d]; !ok {
			return fmt.Errorf("unknown day %q (want mon..sun)", d)
		}
	}
	start, ok := parseClock(w.Start)
	if !ok {
		return fmt.Errorf("start %q is not HH:MM", w.Start)
	}
	end, ok := parseClock(w.End)
	if !ok {
		return fmt.Errorf("end %q is not HH:MM", w.End)
	}
	// A window that wraps midnight is refused rather than interpreted. "fri
	// 22:00-02:00" has no obvious answer for whether Saturday 01:00 is inside it
	// — the day list could mean the day it starts or the day it covers — and
	// guessing would misprice four hours a week in whichever direction we picked.
	// Two windows say it unambiguously, so the file can always express it.
	if end <= start {
		return fmt.Errorf("end %q is not after start %q (a window may not wrap midnight; write two windows)", w.End, w.Start)
	}
	return nil
}

// Contains reports whether t falls inside the window. A window that does not
// Validate contains nothing — the load-time check is what makes that unreachable
// rather than a silent under-bill.
func (w Window) Contains(t time.Time) bool {
	if w.Type != WindowWeekly {
		return false
	}
	loc, ok := w.location()
	if !ok {
		return false
	}
	local := t.In(loc)
	onDay := false
	for _, d := range w.Days {
		if wd, known := weekdays[d]; known && wd == local.Weekday() {
			onDay = true
			break
		}
	}
	if !onDay {
		return false
	}
	start, ok := parseClock(w.Start)
	if !ok {
		return false
	}
	end, ok := parseClock(w.End)
	if !ok {
		return false
	}
	// Minutes since local midnight. Truncating seconds away is what makes the
	// half-open bound behave: 11:59:59 lands on 719 and is inside 09:00-12:00,
	// 12:00:00 lands on 720 and is not.
	mins := local.Hour()*60 + local.Minute()
	return mins >= start && mins < end
}

// location resolves Offset to a fixed zone. Empty and "Z" are UTC.
func (w Window) location() (*time.Location, bool) {
	s := w.Offset
	if s == "" || s == "Z" {
		return time.UTC, true
	}
	if len(s) != 6 || s[3] != ':' || (s[0] != '+' && s[0] != '-') {
		return nil, false
	}
	h, err := strconv.Atoi(s[1:3])
	if err != nil || h > 23 {
		return nil, false
	}
	m, err := strconv.Atoi(s[4:6])
	if err != nil || m > 59 {
		return nil, false
	}
	secs := h*3600 + m*60
	if s[0] == '-' {
		secs = -secs
	}
	return time.FixedZone(s, secs), true
}

// parseClock turns "HH:MM" into minutes since midnight.
func parseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil || h > 23 {
		return 0, false
	}
	m, err := strconv.Atoi(s[3:])
	if err != nil || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// Resolve reduces a cost to the plain rate table that applies to one request:
// the one whose prompt is contextTokens long, sent at instant at.
//
// The result carries neither tiers nor schedules, so it is a flat set of rates
// that pricing.Cost can simply sum.
//
// # A cost may be conditional on size or on time, never both
//
// Tiers and Schedules both state ABSOLUTE rates, so applying one after the other
// would mean the second silently overwrote the first, and there is no published
// evidence about which way a vendor would intend that to compose — no vendor
// songguo maps declares both. Load and config.validatePrices reject a cost
// carrying both, which is what makes the branch below a choice between two
// exclusive cases rather than a precedence rule quietly deciding money.
//
// Schedules are tried first purely so this function stays total if a cost ever
// reaches it unvalidated; it is not a ranking, and it is unreachable.
func (c Cost) Resolve(contextTokens float64, at time.Time) Cost {
	if len(c.Schedules) > 0 {
		return c.atSchedule(at)
	}
	return c.atTier(contextTokens)
}

// atTier resolves the rates for a request whose input is contextTokens long.
//
// A vendor prices the WHOLE request at the bracket its prompt falls into — it is
// not marginal, so crossing 272k does not mean "the first 272k stay cheap". The
// highest crossed threshold wins and only that one applies; brackets do not
// compound.
//
// Tiers are stated in ascending order upstream, but this does not rely on that:
// depending on an ordering nobody guarantees would silently misprice if it ever
// changed, and picking the maximum explicitly costs one comparison.
//
// Media axes are untouched — every tier upstream carries token rates.
func (c Cost) atTier(contextTokens float64) Cost {
	best := -1
	for i, t := range c.Tiers {
		if t.Tier.Type != TierContext || contextTokens <= float64(t.Tier.Size) {
			continue
		}
		if best < 0 || t.Tier.Size > c.Tiers[best].Tier.Size {
			best = i
		}
	}
	out := c.flat()
	if best < 0 {
		return out
	}
	t := c.Tiers[best]
	out.override(t.Input, t.Output, t.CacheRead, t.CacheWrite)
	return out
}

// atSchedule resolves the rates in force at instant at.
//
// The FIRST matching schedule wins. Windows are half-open so two that meet do
// not overlap, and a vendor publishes one peak rate rather than a stack of them;
// taking the first keeps the answer independent of map iteration or file order,
// which a "highest wins" rule would not be if two schedules ever tied.
//
// No match is the ordinary case — off-peak is most of the week — and it means
// the base rates stand, exactly as an uncrossed tier does.
func (c Cost) atSchedule(at time.Time) Cost {
	out := c.flat()
	for _, s := range c.Schedules {
		for _, w := range s.When {
			if w.Contains(at) {
				out.override(s.Input, s.Output, s.CacheRead, s.CacheWrite)
				return out
			}
		}
	}
	return out
}

// flat is the cost with its conditional tables dropped, leaving the base rates.
func (c Cost) flat() Cost {
	out := c
	out.Tiers = nil
	out.Schedules = nil
	return out
}

// override applies the axes a tier or schedule states, leaving the ones it omits
// at the base rate. Zero means "unstated": both forms state only what changes,
// and reading a missing axis as "unchanged" changes only what the vendor said
// changed. A vendor that genuinely drops an axis to free in a bracket cannot be
// expressed, and none does.
func (c *Cost) override(input, output, cacheRead, cacheWrite float64) {
	if input != 0 {
		c.Input = input
	}
	if output != 0 {
		c.Output = output
	}
	if cacheRead != 0 {
		c.CacheRead = cacheRead
	}
	if cacheWrite != 0 {
		c.CacheWrite = cacheWrite
	}
}

// Equal compares two costs. Cost holds a slice, so == does not apply to it; the
// axes are listed explicitly here rather than reflected over, so adding one
// without updating this fails a test rather than silently comparing less.
func (c Cost) Equal(o Cost) bool {
	if c.Input != o.Input || c.Output != o.Output ||
		c.CacheRead != o.CacheRead || c.CacheWrite != o.CacheWrite ||
		c.Character != o.Character || c.Second != o.Second ||
		c.Image != o.Image || c.Call != o.Call ||
		len(c.Tiers) != len(o.Tiers) || len(c.Schedules) != len(o.Schedules) {
		return false
	}
	for i := range c.Tiers {
		if c.Tiers[i] != o.Tiers[i] {
			return false
		}
	}
	for i := range c.Schedules {
		if !c.Schedules[i].equal(o.Schedules[i]) {
			return false
		}
	}
	return true
}

// equal compares two schedules. CostSchedule holds a window slice and Window
// holds a day slice, so neither is comparable with ==; the axes are spelled out
// here for the same reason Cost.Equal spells its own out, so adding one without
// updating this fails a test instead of silently comparing less.
//
// This is what keeps internal/pricefeed honest: a refresh reports a rate as
// changed by comparing costs, so a schedule the comparison could not see would
// make a peak-rate edit look like no change at all.
func (s CostSchedule) equal(o CostSchedule) bool {
	if s.Input != o.Input || s.Output != o.Output ||
		s.CacheRead != o.CacheRead || s.CacheWrite != o.CacheWrite ||
		len(s.When) != len(o.When) {
		return false
	}
	for i := range s.When {
		if !s.When[i].equal(o.When[i]) {
			return false
		}
	}
	return true
}

func (w Window) equal(o Window) bool {
	if w.Type != o.Type || w.Offset != o.Offset ||
		w.Start != o.Start || w.End != o.End || len(w.Days) != len(o.Days) {
		return false
	}
	return slices.Equal(w.Days, o.Days)
}

// Zero reports whether a cost declares no rate at all, i.e. would always meter
// $0. Distinct from a published zero on a genuinely free tier, which declares a
// rate that happens to be zero — callers that care about provenance must ask
// where the cost came from, not whether it is empty.
func (c Cost) Zero() bool { return c.Equal(Cost{}) }

// Tokens reports whether the cost prices token usage, which is the axis group
// that most models use and the only one models.dev supplies.
func (c Cost) Tokens() bool {
	return c.Input != 0 || c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0
}

// Load parses and merges the embedded catalog. It fails fast if either file is
// malformed, which surfaces at startup/test time rather than to an end user.
//
// The hand-maintained file wins: a model present in both keeps its hand-written
// cost, so pinning a rate the generator would overwrite is a matter of adding it
// to catalog.json.
//
// The merged result is validated, not just parsed. A cost that JSON accepts can
// still be unusable — a window nothing will ever match, or both conditional
// tables at once — and the whole point of a rate table is that nobody looks at it
// again once it is right. Catching that here means a bad edit fails the build;
// the alternative is discovering it in a month of quietly wrong invoices.
func Load() (Catalog, error) {
	gen, err := parse(generated, "models.json")
	if err != nil {
		return nil, err
	}
	man, err := parse(manual, "catalog.json")
	if err != nil {
		return nil, err
	}
	out := merge(gen, man)
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// validate checks every cost the catalogue declares. It reports the FIRST
// problem: these are typos in a file we own, fixed one at a time, and a single
// precise message beats a list.
func (c Catalog) validate() error {
	for _, pid := range sortedKeys(c) {
		p := c[pid]
		for _, mid := range sortedKeys(p.Models) {
			if err := ValidateCost(p.Models[mid].Cost); err != nil {
				return fmt.Errorf("catalog: %s/%s: %w", pid, mid, err)
			}
		}
	}
	return nil
}

// ValidateCost reports what makes a cost unusable, or nil. Exported because
// config runs the same check over the rates that reach it from the store, so an
// operator's row and a catalogue entry are held to one standard.
func ValidateCost(c Cost) error {
	// See Resolve: both forms state absolute rates, so a cost declaring both has
	// no defined meaning rather than a debatable one.
	if len(c.Tiers) > 0 && len(c.Schedules) > 0 {
		return errors.New("declares both context tiers and time schedules; a rate may be conditional on size or on time, not both")
	}
	for i, s := range c.Schedules {
		if len(s.When) == 0 {
			return fmt.Errorf("schedule %d applies to no window", i)
		}
		for j, w := range s.When {
			if err := w.Validate(); err != nil {
				return fmt.Errorf("schedule %d window %d: %w", i, j, err)
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Manual returns just the hand-maintained half — catalog.json, before the
// generated models are merged in. It exists for tooling that needs to know what
// songguo declares as opposed to what a sync produced: pass the merged catalog
// to modelsdev.Generate and every model looks already pinned, because the merge
// has by then folded the generated entries into the same map.
func Manual() (Catalog, error) { return parse(manual, "catalog.json") }

func parse(raw []byte, name string) (Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("catalog: parse %s: %w", name, err)
	}
	return c, nil
}

// merge overlays the hand-maintained catalog onto the generated one. Providers
// are unioned; within a provider the hand-written topology and any hand-written
// model replace their generated counterparts, and generated models the manual
// file says nothing about are kept.
func merge(gen, man Catalog) Catalog {
	out := make(Catalog, len(gen)+len(man))
	for id, p := range gen {
		p.Models = maps.Clone(p.Models)
		out[id] = p
	}
	for id, m := range man {
		p, ok := out[id]
		if !ok {
			out[id] = m
			continue
		}
		// Identity and topology are the manual file's to state; the generator
		// emits neither, so an empty field there must not blank one here.
		if m.Name != "" {
			p.Name = m.Name
		}
		if m.Doc != "" {
			p.Doc = m.Doc
		}
		if len(m.Env) > 0 {
			p.Env = m.Env
		}
		if m.NPM != "" {
			p.NPM = m.NPM
		}
		p.ID = id
		p.Endpoints = m.Endpoints
		p.Quirks = m.Quirks
		p.Custom = m.Custom
		if p.Models == nil {
			p.Models = make(map[string]Model, len(m.Models))
		}
		maps.Copy(p.Models, m.Models)
		out[id] = p
	}
	return out
}

// versionDot matches a dot used as a version separator: one between two digits.
// The guard on both sides is the whole rule — a bare dot-to-dash rewrite would
// also mangle a dot that separates words.
var versionDot = regexp.MustCompile(`(\d)\.(\d)`)

// CanonicalID is the form two model ids are compared in.
//
// The same model is written two ways in the wild. songguo (and the operator
// typing into the dashboard) carries claude-fable-5.1 and gpt-5.6-sol, where
// models.dev keys them claude-fable-5-1 and gpt-5-6-sol. Comparing the literal
// strings therefore misses, and the miss is expensive: a model that fails to
// match its published rate meters as unpriced.
//
// Lowercasing and rewriting the version dot to a dash collapses both spellings
// onto one key, in one rule that needs no direction. That replaces the older
// approach of deriving candidate spellings per lookup, which only ever
// converted dash to dot and so could not find a dotted id at all.
//
// The substitution runs twice because the pattern consumes the digit on either
// side, so a chain like 2.0.1 needs a second pass for the middle — the same
// reason modelsdev.dots ran twice.
//
// Two DISTINCT models must never meet here: colliding ids would silently merge
// their prices, which is a worse failure than not matching at all.
// TestCanonicalIDNeverCollides holds that against every id models.dev publishes
// under a provider songguo maps.
func CanonicalID(id string) string {
	s := strings.ToLower(id)
	return versionDot.ReplaceAllString(versionDot.ReplaceAllString(s, "$1-$2"), "$1-$2")
}

// dateSuffix matches a trailing -YYYYMMDD release pin, e.g.
// claude-haiku-4-5-20251001.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// LookupIDs returns the canonical forms to try when resolving a model id to a
// published rate, most specific first:
//
//  1. the id itself, so an upstream that lists the dated build separately (both
//     claude-haiku-4-5 and claude-haiku-4-5-20251001 exist) wins on its own row;
//  2. the id with its trailing -YYYYMMDD release pin dropped, so a dated id the
//     upstream does NOT list still finds the undated model it is a build of.
//
// Order is the whole point. Falling back to the undated rate is right when
// nothing more specific exists and wrong when it does — vendors reprice between
// builds, so a dated id must never take a sibling build's number while its own
// is published.
func LookupIDs(id string) []string {
	canon := CanonicalID(id)
	out := []string{canon}
	if undated := dateSuffix.ReplaceAllString(canon, ""); undated != canon {
		out = append(out, undated)
	}
	return out
}

// DeclaredModels returns the model ids a provider's endpoints serve, deduped.
// It is what the dashboard offers as the preset's suggested models; pricing does
// NOT consult it. modelsdev.Generate prices every model the upstream publishes,
// so a model missing from this list still meters correctly — which is the whole
// point, since a vendor ships models faster than this file is edited.
func (p Provider) DeclaredModels() []string {
	seen := make(map[string]bool)
	var out []string
	for _, ep := range p.Endpoints {
		for _, m := range ep.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
