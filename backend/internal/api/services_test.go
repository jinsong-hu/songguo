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
