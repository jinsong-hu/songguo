// Package modelsdev reads model metadata and prices from models.dev's public
// api.json and takes every model published by the providers songguo maps.
//
// It is the source behind both cmd/catalogsync (which writes models.json at
// build time) and internal/pricefeed (which refreshes prices in a running
// gateway). Neither path lets a fetched rate reach metering unchecked — see
// pricefeed for the gates.
//
// # models.dev is the base; catalog.json is the override
//
// The published list is the floor, and the hand-written catalog.json states only
// what models.dev cannot: the routing topology, the models with no first-party
// upstream (every Volcengine one), and any rate an operator wants pinned. So a
// model a vendor ships today is priced by the next refresh, without an edit, a
// rebuild or a deploy. See Generate for why that direction is the safe one.
//
// # Why models.dev rather than a model-first list
//
// models.dev is keyed by PROVIDER, so a price under "deepseek" is DeepSeek's
// own rate. A model-first list has to be asked which reseller a price belongs
// to, and answering that wrong is expensive: OpenRouter's top-level entry for
// deepseek-v4-pro is Novita's 1.168/2.336 against DeepSeek's own 0.435/0.870.
// Provider-first removes the question rather than answering it.
//
// It also splits mainland-China and international price lists explicitly
// (alibaba / alibaba-cn, minimax / minimax-cn, moonshotai / moonshotai-cn),
// which is a real 4.6x difference on qwen-max and not something to guess at.
//
// # No translation
//
// internal/catalog's types mirror models.dev's schema, so this package decodes
// api.json straight into catalog.Provider. There is no field mapping, no unit
// conversion and no rounding — which is why a re-sync produces an empty diff
// instead of float noise.
package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/songguo/songguo/internal/catalog"
)

// DefaultURL is models.dev's public catalogue. No credential is required.
const DefaultURL = "https://models.dev/api.json"

// providerFor maps a songguo catalog provider id to the models.dev provider whose
// price list applies to it. A provider absent from this map is never generated:
// that is the mechanism keeping the hand-maintained half hand-maintained.
//
// The Chinese vendors resolve to the INTERNATIONAL lists, matching the rates
// songguo already carried (alibaba's qwen-max at 1.6/6.4, not alibaba-cn's
// 0.345/1.377). moonshotai and minimax publish identical -cn and international
// rates, so the choice is moot for those two.
//
// Volcengine is deliberately absent: models.dev has no first-party provider for
// it, and its Doubao models appear only under resellers (NanoGPT lists
// doubao-seed-2-0-pro-260215 at 0.782/3.876 against Volcengine's own 0.44/2.22).
// Those stay in catalog.json.
var providerFor = map[string]string{
	"openai":       "openai",
	"azure-openai": "azure",
	"anthropic":    "anthropic",
	"xai":          "xai",
	"deepseek":     "deepseek",
	"dashscope":    "alibaba",
	"moonshot":     "moonshotai",
	"minimax":      "minimax",
	"zhipu":        "zhipuai",
}

// Skip records something the generator left alone, and why, so the sync report
// names the hand-maintained half rather than leaving it as a silent absence.
// Model is empty when the whole provider was skipped.
type Skip struct {
	Provider string
	Model    string
	Reason   string
}

// Client reads models.dev.
type Client struct {
	HTTP *http.Client
	URL  string
}

// New returns a Client with a timeout generous enough for the 3.5 MB payload on
// a slow link.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, URL: DefaultURL}
}

// Fetch downloads and decodes api.json. The result is keyed by models.dev
// provider id and decodes directly into catalog's types — models.dev fields we
// do not model (description, tiers, per-tier overrides) are ignored by
// encoding/json.
func (c *Client) Fetch(ctx context.Context) (map[string]catalog.Provider, error) {
	url := c.URL
	if url == "" {
		url = DefaultURL
	}
	var out map[string]catalog.Provider
	if err := c.get(ctx, url, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("modelsdev: api.json returned no providers")
	}
	return out, nil
}

// Generate builds the generated half of the catalog from an upstream snapshot.
//
// For each mapped provider it emits EVERY model models.dev lists under it. A
// model the hand-written file already prices is skipped, so catalog.json always
// wins and stays the place to pin a rate.
//
// # Why the whole list, and not the models we declare
//
// This used to iterate hand[id].DeclaredModels() — the ids written on
// catalog.json's endpoints — so models.dev was only ever queried, never
// enumerated. That made pricing a new model a RELEASE: edit catalog.json, run
// catalogsync, rebuild, redeploy. In the gap, a model a vendor shipped last week
// metered as unpriced however promptly models.dev published its rate, and the
// fallback pass then lent it a sibling's number. gpt-6-astra arrived priced at
// 10/50 upstream and billed at 5/30; grok-4.6 billed at zero. Enumerating
// upstream makes a new model a config change instead.
//
// The cost of the old rule was never paid for by the thing it bought. Carrying a
// price for a model nobody routes is free — it is one row nothing reads — while
// missing one silently misbills every call.
//
// # Why NOT every provider models.dev carries
//
// providerFor stays exactly as it is, and this is the load-bearing restriction.
// models.dev lists 213 providers; 1,081 model ids appear under more than one and
// 834 of those DISAGREE on price, because most are resellers. deepseek-v4-pro is
// 0.435/0.87 under deepseek's own list and up to 1.215/3.645 under a reseller's.
// Enumerating the nine first-party lists is what keeps a price attributable to
// the vendor that set it; widening the map would trade a missing price for a
// wrong one, which is the worse failure.
func Generate(upstream map[string]catalog.Provider, hand catalog.Catalog) (catalog.Catalog, []Skip) {
	out := make(catalog.Catalog)
	var skips []Skip

	for _, id := range sortedKeys(hand) {
		ours := hand[id]

		mdID, mapped := providerFor[id]
		if !mapped {
			skips = append(skips, Skip{id, "", "provider has no models.dev counterpart"})
			continue
		}
		up, ok := upstream[mdID]
		if !ok {
			skips = append(skips, Skip{id, "", "models.dev has no provider " + mdID})
			continue
		}

		models := make(map[string]catalog.Model)
		for _, m := range sortedKeys(up.Models) {
			// Pinning is matched on the canonical id so a rate hand-pinned as
			// glm-4.5 is not overwritten by an upstream glm-4-5. Without that,
			// TestGenerateNeverEmitsAPinnedModel's guarantee would hold only for
			// ids that happen to be spelled identically on both sides.
			if pinnedID, pinned := pinnedAs(ours, m); pinned {
				skips = append(skips, Skip{id, pinnedID, "pinned in catalog.json"})
				continue
			}
			found := up.Models[m]
			// The key is models.dev's own string. Lookups canonicalize both
			// sides (catalog.CanonicalID), so an operator who types the dotted
			// spelling still resolves — there is no need to guess here which of
			// the two forms a client will send.
			found.ID = m
			models[m] = found
		}
		if len(models) == 0 {
			continue
		}
		out[id] = catalog.Provider{
			ID: id, Name: up.Name, Doc: up.Doc, Env: up.Env, NPM: up.NPM, Models: models,
		}
	}
	return out, skips
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	var err error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * time.Second):
			}
		}
		if err = c.getOnce(ctx, url, out); err == nil {
			return nil
		}
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", attempts, err)
}

// attempts is how many times the fetch is tried before giving up. One transient
// EOF should not abandon a sync, and should certainly not fail the drift guard
// in a way that reads as a price change.
//
// This is not the "songguo never invents retries" rule bending: that invariant
// is about forwarding a caller's request, where a second attempt can replay a
// side effect nobody asked to repeat. Re-reading a public price list has no side
// effect and no caller.
const attempts = 3

func (c *Client) getOnce(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("modelsdev: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &statusError{code: resp.StatusCode, status: resp.Status, url: url}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("modelsdev: decode: %w", err)
	}
	return nil
}

// statusError is a non-200 response, kept typed so retryable can tell a rate
// limit from a URL that simply does not exist.
type statusError struct {
	code   int
	status string
	url    string
}

func (e *statusError) Error() string { return fmt.Sprintf("modelsdev: GET %s: %s", e.url, e.status) }

// retryable reports whether another attempt could plausibly succeed. Transport
// errors and truncated bodies are worth repeating; a 404 is not.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return true
}

// pinnedAs reports whether catalog.json prices a model that is the same model as
// the given upstream id, and under which spelling it wrote it.
//
// The comparison is canonical rather than literal because the two files spell
// versions differently: a rate pinned as glm-4.5 must still shield an upstream
// glm-4-5. A literal match would let the generator emit the upstream twin
// alongside the pin, and since merge() lets the hand-written entry win, the
// result would be a generated row that is never read — invisible, but exactly
// the "the feed overwrote a rate someone typed on purpose" failure if the merge
// order ever changed.
func pinnedAs(ours catalog.Provider, upstreamID string) (string, bool) {
	want := catalog.CanonicalID(upstreamID)
	for id := range ours.Models {
		if catalog.CanonicalID(id) == want {
			return id, true
		}
	}
	return "", false
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
