package catalog

import (
	"testing"

	"github.com/songguo/songguo/internal/wire"
)

// TestCatalogLoads parses and merges the embedded files and checks structural
// invariants: every endpoint names a registered wire and a non-empty URL +
// adapter, and every model an endpoint suggests is defined in the provider's
// model map.
//
// That last check changed meaning when cmd/catalogsync started generating every
// model an upstream publishes rather than only the ones the endpoints name. It
// no longer detects a stale SYNC — a model an endpoint omits is now priced
// anyway. It detects a stale catalog.json: an endpoint suggesting a model
// models.dev has retired, which is a dead entry in the dashboard's picker.
func TestCatalogLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c) == 0 {
		t.Fatal("no providers in catalog")
	}

	knownAdapter := map[string]bool{
		"openai-compatible":    true,
		"anthropic-compatible": true,
		"volc-speech":          true,
	}

	canon := make(map[string]map[string]bool, len(c))
	for id, p := range c {
		set := make(map[string]bool, len(p.Models))
		for mid := range p.Models {
			set[CanonicalID(mid)] = true
		}
		canon[id] = set
	}

	for id, p := range c {
		if id == "" || p.Name == "" {
			t.Errorf("provider with empty id/name: %q %+v", id, p)
		}
		if p.ID != id {
			t.Errorf("provider %q carries id %q; key and id field must agree", id, p.ID)
		}
		if len(p.Endpoints) == 0 {
			t.Errorf("provider %q has no endpoints", id)
		}
		for _, ep := range p.Endpoints {
			if _, ok := wire.Get(ep.Wire); !ok {
				t.Errorf("provider %q endpoint references unknown wire %q", id, ep.Wire)
			}
			if ep.Endpoint == "" {
				t.Errorf("provider %q endpoint %q has empty endpoint URL", id, ep.Wire)
			}
			if !knownAdapter[ep.Adapter] {
				t.Errorf("provider %q endpoint %q has unknown adapter %q", id, ep.Wire, ep.Adapter)
			}
			for _, m := range ep.Models {
				// Canonically, because the two files legitimately spell a
				// version differently: catalog.json carries the dotted form a
				// client sends, models.json carries models.dev's key. A literal
				// compare would report drift where there is none.
				if _, ok := canon[id][CanonicalID(m)]; !ok {
					t.Errorf("provider %q endpoint %q suggests model %q, which models.dev no longer publishes — drop it from catalog.json", id, ep.Wire, m)
				}
			}
		}
		for mid, m := range p.Models {
			if m.ID != mid {
				t.Errorf("provider %q model %q carries id %q; key and id field must agree", id, mid, m.ID)
			}
		}
	}
}

// TestManualWinsOverGenerated pins the merge rule that makes catalog.json the
// place to pin a rate: a model present in both files keeps its hand-written
// cost, and the generated sibling is still kept.
func TestManualWinsOverGenerated(t *testing.T) {
	gen := Catalog{"acme": {ID: "acme", Name: "Generated", Models: map[string]Model{
		"m1": {ID: "m1", Cost: Cost{Input: 1, Output: 2}},
		"m2": {ID: "m2", Cost: Cost{Input: 3, Output: 4}},
	}}}
	man := Catalog{"acme": {ID: "acme", Name: "Acme", Models: map[string]Model{
		"m1": {ID: "m1", Cost: Cost{Input: 99}},
	}}}

	got := merge(gen, man)
	if n := len(got["acme"].Models); n != 2 {
		t.Fatalf("merged models = %d, want 2 (generated m2 kept alongside pinned m1)", n)
	}
	if c := got["acme"].Models["m1"].Cost; c.Input != 99 {
		t.Errorf("m1 cost = %+v, want the hand-written 99", c)
	}
	if c := got["acme"].Models["m2"].Cost; c.Input != 3 {
		t.Errorf("m2 cost = %+v, want the generated 3", c)
	}
	if got["acme"].Name != "Acme" {
		t.Errorf("name = %q, want the hand-written one", got["acme"].Name)
	}
}

// TestMergeKeepsGeneratedIdentityWhenManualIsSilent guards the other direction:
// catalog.json states topology and often says nothing about a provider's name or
// docs, and an empty field there must not blank the generated value.
func TestMergeKeepsGeneratedIdentityWhenManualIsSilent(t *testing.T) {
	gen := Catalog{"acme": {ID: "acme", Name: "Acme Inc", Doc: "https://acme.example/docs"}}
	man := Catalog{"acme": {ID: "acme", Endpoints: []Endpoint{
		{Wire: "openai/chat", Endpoint: "https://x", Adapter: "openai-compatible"},
	}}}

	got := merge(gen, man)["acme"]
	if got.Name != "Acme Inc" || got.Doc != "https://acme.example/docs" {
		t.Errorf("merged identity = %q/%q, want the generated values preserved", got.Name, got.Doc)
	}
	if len(got.Endpoints) != 1 {
		t.Errorf("merged endpoints = %d, want the hand-written 1", len(got.Endpoints))
	}
}

// TestCanonicalID pins the rule that lets the two spellings of a model id meet.
func TestCanonicalID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The version dot becomes a dash, which is the whole job.
		{"claude-fable-5.1", "claude-fable-5-1"},
		{"claude-fable-5-1", "claude-fable-5-1"},
		{"gpt-5.6-sol", "gpt-5-6-sol"},
		{"GPT-5.6-Sol", "gpt-5-6-sol"},
		// Twice, because the pattern consumes the digit on either side and a
		// chain would otherwise keep its middle separator.
		{"doubao-seed-2.0.1-pro", "doubao-seed-2-0-1-pro"},
		// A dot NOT between two digits is not a version separator and stays.
		{"gpt-4o.preview", "gpt-4o.preview"},
		// Already-dashed and dotless ids are untouched.
		{"gpt-5-mini", "gpt-5-mini"},
		{"gpt-6-astra", "gpt-6-astra"},
		{"text-embedding-3-small", "text-embedding-3-small"},
	} {
		if got := CanonicalID(tc.in); got != tc.want {
			t.Errorf("CanonicalID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLookupIDsPrefersTheDatedBuild guards the ordering rule: a dated id must
// take its OWN published rate when the upstream lists it, and fall back to the
// undated model only when it does not. Vendors reprice between builds, so the
// reverse order would quietly bill a build at a sibling's rate.
func TestLookupIDsPrefersTheDatedBuild(t *testing.T) {
	got := LookupIDs("claude-haiku-4-5-20251001")
	want := []string{"claude-haiku-4-5-20251001", "claude-haiku-4-5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LookupIDs = %v, want %v", got, want)
	}
	// An id with no date pin yields exactly one form — no speculative second
	// lookup that could match something else.
	if got := LookupIDs("gpt-6-astra"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Fatalf("LookupIDs(gpt-6-astra) = %v, want [gpt-6-astra]", got)
	}
	// A trailing number that is not 8 digits is a version, not a date pin.
	if got := LookupIDs("qwen3.8-max-0902"); len(got) != 1 {
		t.Fatalf("LookupIDs(qwen3.8-max-0902) = %v, want one form", got)
	}
}

// TestCanonicalIDNeverCollides is the safety net under CanonicalID. Two distinct
// models meeting under the rule would silently merge their prices — one would
// bill at the other's rate with nothing to show for it — which is strictly worse
// than the missed match the rule exists to fix.
//
// It runs over the embedded catalog, which since the generator began taking each
// mapped provider's whole published list is a full copy of what models.dev names
// under those providers. So a future upstream id that collides with one we
// already carry fails here at the next `make catalog-sync`, before it can reach
// a price.
func TestCanonicalIDNeverCollides(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checked := 0
	for id, p := range c {
		seen := make(map[string]string, len(p.Models))
		for mid := range p.Models {
			checked++
			k := CanonicalID(mid)
			if prev, dup := seen[k]; dup {
				t.Errorf("provider %q: %q and %q both canonicalize to %q — their prices would merge", id, prev, mid, k)
				continue
			}
			seen[k] = mid
		}
	}
	if checked == 0 {
		t.Fatal("no models checked — this test is vacuous")
	}
	t.Logf("checked %d model id(s) for canonical collisions", checked)
}

// TestDeclaredModels covers the dashboard's suggested-model list for a preset,
// so it must dedupe across endpoints that share a model and skip companion
// wires that serve none. Pricing no longer reads it — modelsdev.Generate prices
// every model the upstream publishes — so a gap here costs a suggestion, not a
// rate.
func TestDeclaredModels(t *testing.T) {
	p := Provider{Endpoints: []Endpoint{
		{Wire: "openai/chat", Models: []string{"a", "b"}},
		{Wire: "openai/responses", Models: []string{"b", "c"}},
		{Wire: "openai/models"},
	}}
	got := p.DeclaredModels()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("DeclaredModels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DeclaredModels() = %v, want %v", got, want)
		}
	}
}

// TestSpeechModelsPriceOnTheirOwnAxes pins the reason Cost was extended beyond
// models.dev's fields: models.dev has no per-character or per-second field of
// any kind, so these entries live only in the hand-written file and must carry a
// media rate. A token rate here would meter $0, since a speech call reports
// characters and seconds and no tokens at all.
func TestSpeechModelsPriceOnTheirOwnAxes(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := c["volcengine-speech"]
	if !ok {
		t.Fatal("volcengine-speech missing from the catalog")
	}
	if len(p.Models) == 0 {
		t.Fatal("volcengine-speech has no models")
	}
	for mid, m := range p.Models {
		if m.Cost.Tokens() {
			t.Errorf("%s prices tokens; speech wires meter characters and seconds", mid)
		}
		if m.Cost.Character == 0 && m.Cost.Second == 0 {
			t.Errorf("%s declares neither a character nor a second rate; it would meter $0", mid)
		}
	}
}

// weeklyPeak is the window shape DeepSeek publishes, for the tests below.
func weeklyPeak(start, end string) Window {
	return Window{
		Type: WindowWeekly, Offset: "+08:00",
		Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: start, End: end,
	}
}

// TestValidateCostRejectsUnusableSchedules is the guard that makes the strictness
// in Window.Validate worth having. Every case here is valid JSON that would
// otherwise load fine and then quietly match nothing — which does not fail, it
// bills every peak hour at the off-peak rate for as long as nobody checks.
func TestValidateCostRejectsUnusableSchedules(t *testing.T) {
	ok := weeklyPeak("09:00", "12:00")

	tests := []struct {
		name string
		cost Cost
	}{
		{
			// See Resolve: both forms state absolute rates, so a cost carrying
			// both has no defined rate rather than a debatable one.
			name: "tiers and schedules together",
			cost: Cost{
				Input:     1,
				Tiers:     []CostTier{{Input: 2, Tier: TierBound{Type: TierContext, Size: 200000}}},
				Schedules: []CostSchedule{{Input: 3, When: []Window{ok}}},
			},
		},
		{
			name: "schedule with no window",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2}}},
		},
		{
			name: "unknown window type",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: "monthly", Offset: "+08:00", Days: []string{"mon"}, Start: "09:00", End: "12:00"},
			}}}},
		},
		{
			name: "offset is a timezone name",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "Asia/Shanghai", Days: []string{"mon"}, Start: "09:00", End: "12:00"},
			}}}},
		},
		{
			name: "misspelled day",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "+08:00", Days: []string{"monday"}, Start: "09:00", End: "12:00"},
			}}}},
		},
		{
			name: "no days at all",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "+08:00", Start: "09:00", End: "12:00"},
			}}}},
		},
		{
			name: "clock is not HH:MM",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "+08:00", Days: []string{"mon"}, Start: "9:00", End: "12:00"},
			}}}},
		},
		{
			// A wrapping window is refused rather than interpreted: whether the
			// day list means the day it starts or the day it covers has no
			// obvious answer, and either guess misprices hours a week.
			name: "window wraps midnight",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "+08:00", Days: []string{"fri"}, Start: "22:00", End: "02:00"},
			}}}},
		},
		{
			name: "window is empty",
			cost: Cost{Input: 1, Schedules: []CostSchedule{{Input: 2, When: []Window{
				{Type: WindowWeekly, Offset: "+08:00", Days: []string{"mon"}, Start: "09:00", End: "09:00"},
			}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCost(tt.cost); err == nil {
				t.Error("ValidateCost accepted a cost that can never price correctly")
			}
		})
	}

	// The shape the catalogue actually uses must pass, or the guard above is just
	// rejecting everything.
	good := Cost{Input: 0.15, Output: 0.6, Schedules: []CostSchedule{
		{Input: 0.3, Output: 1.2, When: []Window{weeklyPeak("09:00", "12:00"), weeklyPeak("14:00", "18:00")}},
	}}
	if err := ValidateCost(good); err != nil {
		t.Errorf("ValidateCost rejected DeepSeek's real shape: %v", err)
	}
}

// TestDeepSeekPeakIsDoubleOffPeak ties the JSON to the vendor's own words. Every
// DeepSeek axis doubles at peak — the pricing page states it outright ("空闲时段
// 价格为高峰时段价格的一半") — so a hand edit that updates the base and forgets
// the schedule, or the reverse, is caught here rather than by an invoice.
func TestDeepSeekPeakIsDoubleOffPeak(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := c["deepseek"]
	if !ok {
		t.Fatal("deepseek missing from the catalog")
	}
	if len(p.Models) == 0 {
		t.Fatal("deepseek declares no models; its rates must be pinned, not generated")
	}
	for mid, m := range p.Models {
		if len(m.Cost.Schedules) != 1 {
			t.Errorf("%s has %d schedules, want exactly the one peak block", mid, len(m.Cost.Schedules))
			continue
		}
		s := m.Cost.Schedules[0]
		if len(s.When) != 2 {
			t.Errorf("%s peak covers %d windows, want 2 (09:00-12:00 and 14:00-18:00)", mid, len(s.When))
		}
		for _, axis := range []struct {
			name       string
			base, peak float64
		}{
			{"input", m.Cost.Input, s.Input},
			{"output", m.Cost.Output, s.Output},
			{"cache_read", m.Cost.CacheRead, s.CacheRead},
		} {
			if axis.base == 0 || axis.peak == 0 {
				t.Errorf("%s: %s is unpriced (base %v, peak %v)", mid, axis.name, axis.base, axis.peak)
				continue
			}
			if got := axis.peak / axis.base; got < 1.999 || got > 2.001 {
				t.Errorf("%s: peak %s is %vx the off-peak rate, want 2x (%v vs %v)",
					mid, axis.name, got, axis.peak, axis.base)
			}
		}
	}
}

// The retired ids DeepSeek still accepts must stay priced. They are served by
// V4.1 Flash and billed at Flash price, so a service still pointing at one meters
// correctly — dropping them would strand it on the provider-ceiling fallback.
func TestDeepSeekRetiredAliasesPriceAsFlash(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	models := c["deepseek"].Models
	flash, ok := models["deepseek-flash"]
	if !ok {
		t.Fatal("deepseek-flash missing; it is the id DeepSeek's API now takes")
	}
	for _, alias := range []string{"deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		m, ok := models[alias]
		if !ok {
			t.Errorf("%s is missing; DeepSeek still accepts it and bills it at Flash price", alias)
			continue
		}
		if !m.Cost.Equal(flash.Cost) {
			t.Errorf("%s costs %+v, want deepseek-flash's %+v", alias, m.Cost, flash.Cost)
		}
		if m.Note == "" {
			t.Errorf("%s carries no note; a retired id that still bills needs to say so", alias)
		}
	}
}
