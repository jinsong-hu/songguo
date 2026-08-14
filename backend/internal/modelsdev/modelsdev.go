// Package modelsdev reads model metadata and prices from models.dev's public
// api.json and selects the entries songguo carries.
//
// It is the source behind both cmd/catalogsync (which writes models.json at
// build time) and internal/pricefeed (which refreshes prices in a running
// gateway). Neither path lets a fetched rate reach metering unchecked — see
// pricefeed for the gates.
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
	"regexp"
	"slices"
	"sort"
	"strings"
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

// aliases pins a songguo model id to a models.dev key when the derivation rules
// in slugsFor cannot get there. Empty today — the dash-to-dot and date-suffix
// rules cover every model we carry — and it exists so the next irregularly named
// model has an obvious home instead of a new special case.
var aliases = map[string]string{}

// Skip records a model the generator left alone, and why. Every declared model
// that is not generated produces one, so the report names the hand-maintained
// half rather than leaving it as a silent absence.
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
// For each mapped provider it emits exactly the models the hand-written catalog
// declares through its endpoints — songguo carries the models it routes, not
// every model an upstream happens to list. A model the hand-written file already
// prices is skipped, so catalog.json always wins and stays the place to pin a
// rate.
func Generate(upstream map[string]catalog.Provider, hand catalog.Catalog) (catalog.Catalog, []Skip) {
	out := make(catalog.Catalog)
	var skips []Skip

	for _, id := range sortedKeys(hand) {
		ours := hand[id]
		declared := ours.DeclaredModels()
		sort.Strings(declared)

		mdID, mapped := providerFor[id]
		if !mapped {
			for _, m := range declared {
				skips = append(skips, Skip{id, m, "provider has no models.dev counterpart"})
			}
			continue
		}
		up, ok := upstream[mdID]
		if !ok {
			for _, m := range declared {
				skips = append(skips, Skip{id, m, "models.dev has no provider " + mdID})
			}
			continue
		}

		models := make(map[string]catalog.Model)
		for _, m := range declared {
			if _, pinned := ours.Models[m]; pinned {
				skips = append(skips, Skip{id, m, "pinned in catalog.json"})
				continue
			}
			found, ok := lookup(up.Models, m)
			if !ok {
				skips = append(skips, Skip{id, m, "not listed by models.dev/" + mdID})
				continue
			}
			// Key and id are songguo's model string — what a client actually
			// sends and what routing matches. models.dev's key differs only in
			// the dotted version forms (claude-opus-4.8 vs claude-opus-4-8).
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

var (
	dateSuffix = regexp.MustCompile(`-\d{8}$`)
	digitDash  = regexp.MustCompile(`(\d)-(\d)`)
)

// lookup finds the models.dev entry for a songguo model id, trying the id as
// written before any rewriting so a literal match always wins.
func lookup(models map[string]catalog.Model, id string) (catalog.Model, bool) {
	lower := make(map[string]catalog.Model, len(models))
	for k, v := range models {
		lower[strings.ToLower(k)] = v
	}
	for _, c := range slugsFor(id) {
		if m, ok := lower[c]; ok {
			return m, true
		}
	}
	return catalog.Model{}, false
}

// slugsFor derives the models.dev keys to try for a songguo model id, in
// preference order and lowercased. Three rules cover everything we carry:
//
//   - the id verbatim, which is most of them;
//   - a dash between two digits is a version separator models.dev writes as a
//     dot (claude-opus-4-8 -> claude-opus-4.8). The guard on both sides is the
//     point: a bare dash-before-digit rule would mangle gpt-5-mini;
//   - a trailing -YYYYMMDD pin is dropped (claude-haiku-4-5-20251001).
//
// Anything left over goes in aliases.
func slugsFor(id string) []string {
	if a, ok := aliases[id]; ok {
		return []string{strings.ToLower(a)}
	}
	noDate := dateSuffix.ReplaceAllString(id, "")
	var out []string
	for _, form := range []string{id, dots(id), noDate, dots(noDate)} {
		s := strings.ToLower(form)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// dots rewrites version dashes. Applied twice because the pattern consumes the
// digit on both sides, so a chain like 4-5-6 needs a second pass for the middle.
func dots(s string) string {
	return digitDash.ReplaceAllString(digitDash.ReplaceAllString(s, "$1.$2"), "$1.$2")
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
