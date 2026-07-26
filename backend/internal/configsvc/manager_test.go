package configsvc

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A complete, enabled provider is routable; an incomplete or disabled one is
// skipped without failing the whole snapshot.
func TestManagerSkipsIncompleteProviders(t *testing.T) {
	st := openTestStore(t)

	// Complete + enabled → routes.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "good", Enabled: true, APIKey: "sk-a",
		Models:    []store.ProviderModel{{Model: "gpt-4o", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// No API key → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "nokeys", Enabled: true,
		Models:    []store.ProviderModel{{Model: "m1", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://x.example.com/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "off", Enabled: false, APIKey: "sk-b",
		Models:    []store.ProviderModel{{Model: "m2", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://y.example.com/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snap := m.Current()
	if got := len(snap.Vendors()); got != 1 {
		t.Fatalf("routable vendors = %d, want 1 (incomplete/disabled skipped)", got)
	}
	if _, ok := snap.Vendor("good"); !ok {
		t.Error("expected 'good' to be routable")
	}
	vs := snap.VendorsForModel("gpt-4o")
	if len(vs) != 1 || vs[0].Adapter != "openai-compatible" {
		t.Errorf("VendorsForModel(gpt-4o) = %+v", vs)
	}

	// Setting a key on the draft and reloading makes it routable.
	got, _ := st.ListProviders()
	var draftID string
	for _, p := range got {
		if p.Name == "nokeys" {
			draftID = p.ID
		}
	}
	key := "sk-c"
	if _, err := st.UpdateProvider(draftID, store.ProviderUpdate{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m.Current().Vendors()); got != 2 {
		t.Fatalf("after completing draft, vendors = %d, want 2", got)
	}
}

// A model that cannot be priced is warned about (not blocked), and the flavour
// of warning says what happened: an unrecognized unit still meters $0, an
// unpriced model borrows its provider's priciest same-unit rate, and only a
// model with nothing to borrow from is left at $0. The provider routes either
// way, and a well-priced model stays silent.
func TestManagerWarnsOnUnpriceableModels(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "warnme", Enabled: true, APIKey: "sk-z",
		Models: []store.ProviderModel{
			{Model: "free-model", Input: 0, Output: 0, Unit: "per_1m_tokens"}, // unpriced → borrows ok-model
			{Model: "typo-model", Input: 1, Output: 2, Unit: "per_1m_token"},  // unknown unit (missing s)
			{Model: "ok-model", Input: 1, Output: 2, Unit: "per_1m_tokens"},   // fine
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing to borrow from: the only token-family price is itself zero.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "allfree", Enabled: true, APIKey: "sk-y",
		Models: []store.ProviderModel{
			{Model: "lonely-model", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://free.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m, err := NewManager(st, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Warnings are non-fatal: the provider still routes.
	if _, ok := m.Current().Vendor("warnme"); !ok {
		t.Fatal("provider with price warnings must still route")
	}

	out := buf.String()
	if !strings.Contains(out, "free-model") || !strings.Contains(out, "no published price") {
		t.Errorf("expected fallback warning for free-model; logs:\n%s", out)
	}
	if !strings.Contains(out, "borrowed_from=ok-model") {
		t.Errorf("fallback warning should name the model it borrowed from; logs:\n%s", out)
	}
	if !strings.Contains(out, "typo-model") || !strings.Contains(out, "unit not recognized") {
		t.Errorf("expected unknown-unit warning for typo-model; logs:\n%s", out)
	}
	if !strings.Contains(out, "lonely-model") || !strings.Contains(out, "no same-unit price exists") {
		t.Errorf("expected zero-price warning for lonely-model; logs:\n%s", out)
	}
}

// An unpriced model is metered at the most expensive rate its own provider
// charges in the same unit family, copied whole from a real model — not a
// per-field maximum, which would invert the cache-read axis (CachedInput == 0
// means "charge full Input", so max(CachedInput) picks the biggest discount).
func TestManagerFallsBackToPriciestSameFamilyPrice(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "acme", Enabled: true, APIKey: "sk-a",
		Models: []store.ProviderModel{
			{Model: "cheap", Input: 1, Output: 2, CachedInput: 0.1, Unit: "per_1m_tokens"},
			// Priciest by output, and deliberately carries no cache discount.
			{Model: "dear", Input: 5, Output: 25, CachedInput: 0, Unit: "per_1m_tokens"},
			{Model: "brand-new", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.acme.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p, ok := m.Current().PriceFor("acme", "brand-new")
	if !ok {
		t.Fatal("missing price for unpriced model")
	}
	if p.Input != 5 || p.Output != 25 {
		t.Errorf("fallback price = %+v, want dear's 5/25", p)
	}
	if p.CachedInput != 0 {
		t.Errorf("fallback CachedInput = %v, want dear's 0 verbatim (not cheap's 0.1 discount)", p.CachedInput)
	}
	if p.Source != "fallback:dear" {
		t.Errorf("fallback Source = %q, want %q", p.Source, "fallback:dear")
	}
	// Real prices keep their own provenance and values.
	if cheap, _ := m.Current().PriceFor("acme", "cheap"); cheap.Input != 1 || cheap.Source == "fallback:dear" {
		t.Errorf("priced model was disturbed: %+v", cheap)
	}
}

// Units across families price disjoint quantities, so a fallback never crosses
// one: a speech model borrows a speech rate, never a token rate (which would
// compute $0 while claiming a price).
func TestManagerFallbackStaysWithinUnitFamily(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "speech", Enabled: true, APIKey: "sk-s",
		Models: []store.ProviderModel{
			{Model: "chat-big", Input: 10, Output: 50, Unit: "per_1m_tokens"},
			{Model: "asr-cheap", Input: 0.002, Unit: "per_second"},
			{Model: "asr-dear", Input: 0.01, Unit: "per_second"},
			{Model: "asr-new", Input: 0, Unit: "per_second"},
			{Model: "tts-new", Input: 0, Unit: "per_char"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.speech.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	asr, _ := m.Current().PriceFor("speech", "asr-new")
	if asr.Unit != "per_second" || asr.Input != 0.01 || asr.Source != "fallback:asr-dear" {
		t.Errorf("asr-new = %+v, want the priciest per_second rate", asr)
	}
	// per_char has no other priced member: stays $0 rather than borrowing a
	// per_second or token rate.
	tts, _ := m.Current().PriceFor("speech", "tts-new")
	if tts.Input != 0 || tts.Unit != "per_char" {
		t.Errorf("tts-new = %+v, want an untouched zero per_char price", tts)
	}
	if config.IsFallbackPrice(tts) {
		t.Errorf("tts-new must not borrow across unit families, got %q", tts.Source)
	}
}

// A fallback is scoped to its own provider. Borrowing across providers would
// make one provider's bill a function of unrelated config — adding an expensive
// provider would silently re-price another provider's unknown models.
func TestManagerFallbackIsProviderScoped(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "cheapco", Enabled: true, APIKey: "sk-c",
		Models: []store.ProviderModel{
			{Model: "cheapco-known", Input: 0.1, Output: 0.9, Unit: "per_1m_tokens"},
			{Model: "cheapco-new", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://cheap.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	before, _ := m.Current().PriceFor("cheapco", "cheapco-new")
	if before.Output != 0.9 {
		t.Fatalf("cheapco-new = %+v, want its own provider's 0.9 ceiling", before)
	}

	// Adding an expensive, unrelated provider must not move that number.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "dearco", Enabled: true, APIKey: "sk-d",
		Models: []store.ProviderModel{
			{Model: "dearco-flagship", Input: 10, Output: 50, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://dear.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	after, _ := m.Current().PriceFor("cheapco", "cheapco-new")
	if after != before {
		t.Errorf("cheapco-new changed from %+v to %+v after an unrelated provider was added", before, after)
	}
}

// An explicit operator override of zero means "this model is free" and is left
// alone; only never-priced models get a fallback.
func TestManagerFallbackRespectsExplicitZeroOverride(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "freebie", Enabled: true, APIKey: "sk-f",
		Models: []store.ProviderModel{
			{Model: "paid", Input: 3, Output: 15, Unit: "per_1m_tokens"},
			{Model: "on-the-house", Input: 0, Output: 0, Unit: "per_1m_tokens", PriceOverride: true},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://free.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p, _ := m.Current().PriceFor("freebie", "on-the-house")
	if p.Input != 0 || p.Output != 0 {
		t.Errorf("explicit zero override = %+v, want an untouched zero", p)
	}
	if config.IsFallbackPrice(p) {
		t.Errorf("explicit zero override must not be given a fallback, got %q", p.Source)
	}
}

// A zero someone published is a real price meaning "free" — the catalog lists
// genuinely free tiers — and must never be re-priced at the provider's ceiling.
// Only a model nobody ever stated a rate for is eligible for a fallback.
func TestManagerFallbackLeavesCatalogPublishedFreeModelsAlone(t *testing.T) {
	st := openTestStore(t)

	// glm-4.7-flash is published in the catalog at 0.0/0.0; glm-5.1 is the
	// priciest model on the same provider and would be the fallback if the gate
	// keyed on the value rather than the provenance.
	if _, err := st.CreateProvider(store.NewProvider{
		Name:      "zhipu",
		Vendor:    "Zhipu",
		CatalogID: "zhipu",
		Enabled:   true,
		APIKey:    "sk-z",
		Models: []store.ProviderModel{
			{Model: "glm-4.7-flash", Unit: "per_1m_tokens"},
			{Model: "glm-5.1", Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://open.bigmodel.cn/api/paas/v4/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	flash, ok := m.Current().PriceFor("zhipu", "glm-4.7-flash")
	if !ok {
		t.Fatal("missing price for glm-4.7-flash")
	}
	if flash.Input != 0 || flash.Output != 0 {
		t.Errorf("catalog-published free model = %+v, want an untouched 0/0", flash)
	}
	if config.IsFallbackPrice(flash) {
		t.Errorf("a published price of zero must not be treated as unpriced, got %q", flash.Source)
	}
}

// A rate typed against the wrong unit (3.0 per_token is $3,000,000/1M) meters
// as written but is never borrowed, so one typo cannot re-price a provider.
func TestManagerFallbackIgnoresImplausibleRates(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "fatfinger", Enabled: true, APIKey: "sk-t",
		Models: []store.ProviderModel{
			{Model: "oops", Input: 3, Output: 3, Unit: "per_token"},
			{Model: "sane", Input: 2, Output: 8, Unit: "per_1m_tokens"},
			{Model: "new", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://oops.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p, _ := m.Current().PriceFor("fatfinger", "new")
	if p.Source != "fallback:sane" || p.Output != 8 {
		t.Errorf("fallback = %+v, want sane's rate, not the mistyped per_token one", p)
	}
	// The mistyped row itself is untouched — we warn, we don't rewrite.
	if oops, _ := m.Current().PriceFor("fatfinger", "oops"); oops.Input != 3 || oops.Unit != "per_token" {
		t.Errorf("mistyped price was rewritten: %+v", oops)
	}
}

// Two builds of identical config must choose the same fallback: map iteration
// order must not leak into billing.
func TestManagerFallbackIsDeterministic(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "ties", Enabled: true, APIKey: "sk-x",
		Models: []store.ProviderModel{
			{Model: "bbb", Input: 1, Output: 9, Unit: "per_1m_tokens"},
			{Model: "aaa", Input: 4, Output: 9, Unit: "per_1m_tokens"}, // same rank, earlier name
			{Model: "ccc", Input: 2, Output: 9, Unit: "per_1m_tokens"},
			{Model: "unpriced", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://ties.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	first, _ := m.Current().PriceFor("ties", "unpriced")
	if first.Source != "fallback:aaa" {
		t.Errorf("tie broke to %q, want fallback:aaa (lowest name at equal rank)", first.Source)
	}
	for i := 0; i < 5; i++ {
		if err := m.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got, _ := m.Current().PriceFor("ties", "unpriced"); got != first {
			t.Fatalf("reload %d chose %+v, want stable %+v", i, got, first)
		}
	}
}

func TestManagerUsesCatalogPriceUnlessOverridden(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name:      "openai",
		Vendor:    "OpenAI",
		CatalogID: "openai",
		Enabled:   true,
		APIKey:    "sk-a",
		Models: []store.ProviderModel{
			// Stale copied price: because PriceOverride is false, the catalog
			// value for gpt-5.6-luna should be used instead.
			{Model: "gpt-5.6-luna", Input: 99, Output: 99, Unit: "per_1m_tokens"},
			// Explicit override: the stored value should be preserved.
			{Model: "gpt-5.6-sol", Input: 7, Output: 8, Unit: "per_1m_tokens", PriceOverride: true},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	luna, ok := m.Current().PriceFor("openai", "gpt-5.6-luna")
	if !ok {
		t.Fatal("missing catalog-tracked price")
	}
	if luna.Input != 1 || luna.Output != 6 || luna.CachedInput != 0.1 {
		t.Fatalf("gpt-5.6-luna price = %+v, want catalog price", luna)
	}
	sol, ok := m.Current().PriceFor("openai", "gpt-5.6-sol")
	if !ok {
		t.Fatal("missing override price")
	}
	if sol.Input != 7 || sol.Output != 8 {
		t.Fatalf("gpt-5.6-sol price = %+v, want explicit override", sol)
	}
}

func TestManagerBorrowsCatalogPriceForUnlinkedProvider(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name:    "sub2api",
		Vendor:  "Custom",
		Enabled: true,
		APIKey:  "sk-a",
		Models: []store.ProviderModel{
			{Model: "claude-haiku-4-5-20251001", Input: 0, Output: 0, Unit: "per_1m_tokens"},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "anthropic/messages", Endpoint: "https://api.example.com/v1/messages", Adapter: "anthropic-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p, ok := m.Current().PriceFor("sub2api", "claude-haiku-4-5-20251001")
	if !ok {
		t.Fatal("missing borrowed price")
	}
	if p.Input != 1 || p.Output != 5 {
		t.Fatalf("borrowed price = %+v, want catalog price", p)
	}
}

// A provider whose endpoints span two (origin, adapter) groups (e.g. DeepSeek's
// OpenAI and Anthropic surfaces, same host but different auth) expands into two
// routing vendors sharing one key: the primary group keeps the provider name,
// the second gets an adapter suffix.
func TestProviderExpandsByOriginAdapter(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "deepseek", Enabled: true, APIKey: "sk-d",
		Models: []store.ProviderModel{{Model: "deepseek-v4-pro", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{
			{Wire: "openai/chat", Endpoint: "https://api.deepseek.com/chat/completions", Adapter: "openai-compatible"},
			{Wire: "openai/models", Endpoint: "https://api.deepseek.com", Adapter: "openai-compatible"},
			{Wire: "anthropic/messages", Endpoint: "https://api.deepseek.com/anthropic/messages", Adapter: "anthropic-compatible"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snap := m.Current()
	if got := len(snap.Vendors()); got != 2 {
		t.Fatalf("vendors = %d, want 2 (one per (origin, adapter))", got)
	}
	openai, ok := snap.Vendor("deepseek")
	if !ok {
		t.Fatal("expected primary group named 'deepseek'")
	}
	if openai.Origin != "https://api.deepseek.com" || openai.Adapter != "openai-compatible" {
		t.Errorf("primary group = %q/%q", openai.Origin, openai.Adapter)
	}
	if openai.Endpoints["openai/chat"] != "https://api.deepseek.com/chat/completions" {
		t.Errorf("primary openai/chat endpoint = %q", openai.Endpoints["openai/chat"])
	}
	anthro, ok := snap.Vendor("deepseek-anthropic")
	if !ok {
		t.Fatal("expected second group named 'deepseek-anthropic'")
	}
	if anthro.Origin != "https://api.deepseek.com" || anthro.Adapter != "anthropic-compatible" {
		t.Errorf("second group = %q/%q", anthro.Origin, anthro.Adapter)
	}
	if anthro.Endpoints["anthropic/messages"] != "https://api.deepseek.com/anthropic/messages" {
		t.Errorf("second anthropic/messages endpoint = %q", anthro.Endpoints["anthropic/messages"])
	}
	// Both groups carry the shared key and the model.
	if openai.Credential.APIKey != "sk-d" || anthro.Credential.APIKey != "sk-d" {
		t.Error("both groups should share the provider key")
	}
}
