// Command catalogsync regenerates internal/catalog/models.json from models.dev.
// Run it by hand and commit the diff:
//
//	make catalog-sync
//
// It owns models.json entirely and rewrites it from scratch each run, so there
// is nothing hand-written in that file to preserve. Everything hand-maintained —
// the routing topology, and the models models.dev cannot supply — lives in
// catalog.json, which this command only ever READS (to learn which models
// songguo declares) and never writes.
//
// # This is not the update mechanism
//
// A generator that only runs when someone remembers is how a stale rate hides
// for months. internal/pricefeed refreshes prices in the running gateway on a
// timer; this command exists so a fresh checkout, an air-gapped install, and the
// first boot before any refresh all start from sane embedded numbers.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/modelsdev"
)

func main() {
	log.SetFlags(0)
	dir := flag.String("dir", "internal/catalog", "directory holding catalog.json and models.json")
	dry := flag.Bool("n", false, "report what would change without writing")
	flag.Parse()

	if err := run(*dir, *dry); err != nil {
		log.Fatalf("catalogsync: %v", err)
	}
}

func run(dir string, dry bool) error {
	handPath := filepath.Join(dir, "catalog.json")
	genPath := filepath.Join(dir, "models.json")

	raw, err := os.ReadFile(handPath)
	if err != nil {
		return err
	}
	var hand catalog.Catalog
	if err := json.Unmarshal(raw, &hand); err != nil {
		return fmt.Errorf("parse %s: %w", handPath, err)
	}

	upstream, err := modelsdev.New().Fetch(context.Background())
	if err != nil {
		return err
	}
	generated, skips := modelsdev.Generate(upstream, hand)

	// MarshalIndent sorts map keys, so the output is byte-stable across runs and
	// a re-sync with no upstream change produces an empty diff.
	out, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	before, _ := os.ReadFile(genPath)
	report(before, out, generated, skips)

	if string(before) == string(out) {
		fmt.Println("\nmodels.json is already in sync; nothing written.")
		return nil
	}
	if dry {
		fmt.Println("\n-n given, nothing written.")
		return nil
	}
	if err := os.WriteFile(genPath, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n", genPath)
	return nil
}

func report(before, after []byte, gen catalog.Catalog, skips []modelsdev.Skip) {
	n := 0
	for _, p := range gen {
		n += len(p.Models)
	}
	fmt.Printf("generated %d model(s) across %d provider(s); %d left hand-maintained.\n",
		n, len(gen), len(skips))

	var old catalog.Catalog
	if len(before) > 0 {
		_ = json.Unmarshal(before, &old)
	}
	type row struct {
		provider, model, from, to string
	}
	var moved []row
	for _, pid := range sortedKeys(gen) {
		for _, mid := range sortedKeys(gen[pid].Models) {
			nc := gen[pid].Models[mid].Cost
			oc, had := catalog.Cost{}, false
			if op, ok := old[pid]; ok {
				if om, ok := op.Models[mid]; ok {
					oc, had = om.Cost, true
				}
			}
			if !had {
				moved = append(moved, row{pid, mid, "(new)", rate(nc)})
			} else if oc != nc {
				moved = append(moved, row{pid, mid, rate(oc), rate(nc)})
			}
		}
	}
	if len(moved) > 0 {
		fmt.Printf("\nprice changes (%d):\n", len(moved))
		fmt.Printf("  %-14s %-28s %-22s %s\n", "PROVIDER", "MODEL", "BEFORE", "AFTER")
		for _, m := range moved {
			fmt.Printf("  %-14s %-28s %-22s %s\n", m.provider, m.model, m.from, m.to)
		}
	}

	byReason := map[string][]string{}
	for _, s := range skips {
		byReason[s.Reason] = append(byReason[s.Reason], s.Provider+"/"+s.Model)
	}
	if len(byReason) > 0 {
		fmt.Println("\nleft hand-maintained:")
		for _, r := range sortedKeys(byReason) {
			names := byReason[r]
			sort.Strings(names)
			fmt.Printf("  %-46s %3d  %s\n", r, len(names), summarize(names))
		}
	}
}

// rate renders a cost compactly for the change table: the token pair when there
// is one, else whichever media axis the model prices.
func rate(c catalog.Cost) string {
	if c.Tokens() {
		s := fmt.Sprintf("%g / %g", c.Input, c.Output)
		if c.CacheRead != 0 {
			s += fmt.Sprintf(" (cr %g)", c.CacheRead)
		}
		return s
	}
	for _, a := range []struct {
		name string
		v    float64
	}{{"char", c.Character}, {"sec", c.Second}, {"img", c.Image}, {"call", c.Call}} {
		if a.v != 0 {
			return fmt.Sprintf("%g /%s", a.v, a.name)
		}
	}
	return "unpriced"
}

func summarize(names []string) string {
	const max = 4
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", +%d more", len(names)-max)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
