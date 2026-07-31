package api

import (
	"testing"

	"github.com/songguo/songguo/internal/store"
)

func TestServicesDataIncludesConfiguredRoutesForAdmin(t *testing.T) {
	s := newTestStore(t)
	pvd, err := s.CreateProvider(store.NewProvider{
		Name: "pool", Enabled: true, Priority: 1, Weight: 2, APIKey: "sk-a",
		Models: []store.ProviderModel{{Model: "m"}},
		Endpoints: []store.ProviderEndpoint{{
			Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions",
			Adapter: "openai-compatible",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	priority, weight := 3, 5
	if err := s.UpdateProviderModelRouting(
		pvd.ID, "m", &disabled, &priority, true, &weight, true,
	); err != nil {
		t.Fatal(err)
	}

	a := newAPI(Deps{Store: s})
	admin, err := a.servicesData(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(admin) != 1 || len(admin[0].Providers) != 1 {
		t.Fatalf("admin services = %+v", admin)
	}
	route := admin[0].Providers[0]
	if route.Enabled || route.Routable || route.Priority != 3 || route.Weight != 5 ||
		route.DefaultPriority != 1 || route.DefaultWeight != 2 {
		t.Fatalf("admin route = %+v", route)
	}

	user, err := a.servicesData(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(user) != 0 {
		t.Fatalf("consumer services = %+v, want disabled route omitted", user)
	}
}

// Weight 0 parks a provider — configured, routable, taking no share of its tier
// — so it has to survive every layer that used to clamp it up to 1. An omitted
// create weight still means an ordinary provider, and a negative one is a 400
// rather than a silent correction.
func TestProviderWeightZeroThroughAPI(t *testing.T) {
	s := newTestStore(t)
	a := newAPI(Deps{Store: s, Reload: func() error { return nil }})

	base := createProviderReq{
		Name:   "pool",
		APIKey: "sk-a",
		Models: []providerModelReq{{Model: "m"}},
		Endpoints: []providerEndpointReq{{
			Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions",
			Adapter: "openai-compatible",
		}},
	}

	// Omitted weight: an ordinary provider, not a parked one.
	created, err := a.createProviderData(base)
	if err != nil {
		t.Fatalf("create without weight: %v", err)
	}
	if created.Weight != 1 {
		t.Fatalf("weight = %d, want the default 1 when omitted", created.Weight)
	}

	// Explicit 0 on create: parked.
	zero, negative := 0, -1
	parkedReq := base
	parkedReq.Name = "parked"
	parkedReq.Weight = &zero
	parked, err := a.createProviderData(parkedReq)
	if err != nil {
		t.Fatalf("create with weight 0: %v", err)
	}
	if parked.Weight != 0 {
		t.Fatalf("weight = %d, want 0 preserved on create", parked.Weight)
	}

	badReq := base
	badReq.Name = "negative"
	badReq.Weight = &negative
	if _, err := a.createProviderData(badReq); !isBadRequest(err) {
		t.Fatalf("create with weight -1 err = %v, want 400", err)
	}

	// Patching down to 0 parks an existing provider; the view reports it as 0.
	patched, err := a.updateProviderData(created.ID, patchProviderReq{Weight: &zero})
	if err != nil {
		t.Fatalf("patch to 0: %v", err)
	}
	if patched.Weight != 0 {
		t.Fatalf("weight after patch = %d, want 0", patched.Weight)
	}
	if _, err := a.updateProviderData(created.ID, patchProviderReq{Weight: &negative}); !isBadRequest(err) {
		t.Fatalf("patch to -1 err = %v, want 400", err)
	}

	// A per-service override of 0 parks the provider for one model only, and the
	// service view reports the 0 rather than the old clamped 1.
	if err := a.patchServiceProviderRoutingData(parked.ID, patchServiceProviderRoutingReq{
		Model: "m", Weight: &zero,
	}); err != nil {
		t.Fatalf("override to 0: %v", err)
	}
	if err := a.patchServiceProviderRoutingData(parked.ID, patchServiceProviderRoutingReq{
		Model: "m", Weight: &negative,
	}); !isBadRequest(err) {
		t.Fatalf("override to -1 err = %v, want 400", err)
	}

	services, err := a.servicesData(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("services = %+v", services)
	}
	for _, route := range services[0].Providers {
		if route.Weight != 0 {
			t.Fatalf("route %s weight = %d, want 0 reported verbatim", route.Name, route.Weight)
		}
	}
}

func TestPatchServiceProviderRoutingData(t *testing.T) {
	s := newTestStore(t)
	pvd, err := s.CreateProvider(store.NewProvider{
		Name: "pool", Enabled: true, Priority: 1, Weight: 2, APIKey: "sk-a",
		Models: []store.ProviderModel{{Model: "m"}},
		Endpoints: []store.ProviderEndpoint{{
			Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions",
			Adapter: "openai-compatible",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	a := newAPI(Deps{Store: s, Reload: func() error {
		reloads++
		return nil
	}})

	enabled := false
	priority, weight := 4, 7
	if err := a.patchServiceProviderRoutingData(pvd.ID, patchServiceProviderRoutingReq{
		Model: "m", Enabled: &enabled, Priority: &priority, Weight: &weight,
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}

	got, _ := s.GetProvider(pvd.ID)
	route := got.Models[0]
	if route.RoutingEnabled || route.PriorityOverride == nil || *route.PriorityOverride != 4 ||
		route.WeightOverride == nil || *route.WeightOverride != 7 {
		t.Fatalf("stored route = %+v", route)
	}

	if err := a.patchServiceProviderRoutingData(pvd.ID, patchServiceProviderRoutingReq{
		Model: "m", InheritPriority: true, InheritWeight: true,
	}); err != nil {
		t.Fatalf("inherit: %v", err)
	}
	got, _ = s.GetProvider(pvd.ID)
	if got.Models[0].PriorityOverride != nil || got.Models[0].WeightOverride != nil {
		t.Fatalf("stored overrides after inherit = %+v", got.Models[0])
	}
}
