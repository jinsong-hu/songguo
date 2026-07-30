package store

import "testing"

func TestProxyCRUDAndProviderAssignment(t *testing.T) {
	s := openTestStore(t)

	proxy, err := s.CreateProxy(NewProxy{
		Name: "office", Type: ProxyTypeHTTPS, Host: "proxy.example.com", Port: 443,
		Username: "alice", Password: "secret",
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if proxy.ID == "" || proxy.Password != "secret" {
		t.Fatalf("created proxy = %+v", proxy)
	}

	list, err := s.ListProxies()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProxies = %+v, %v", list, err)
	}

	newHost := "proxy2.example.com"
	emptyPassword := ""
	proxy, err = s.UpdateProxy(proxy.ID, ProxyUpdate{
		Host: &newHost, Password: &emptyPassword,
	})
	if err != nil {
		t.Fatalf("UpdateProxy: %v", err)
	}
	if proxy.Host != newHost || proxy.Password != "" {
		t.Fatalf("updated proxy = %+v", proxy)
	}

	provider, err := s.CreateProvider(NewProvider{Name: "p1", ProxyID: proxy.ID})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if provider.ProxyID != proxy.ID {
		t.Fatalf("provider proxy = %q, want %q", provider.ProxyID, proxy.ID)
	}
	proxy, _ = s.GetProxy(proxy.ID)
	if proxy.ProviderCount != 1 {
		t.Fatalf("provider_count = %d, want 1", proxy.ProviderCount)
	}
	if err := s.DeleteProxy(proxy.ID); err == nil {
		t.Fatal("assigned proxy deletion should fail")
	}

	direct := ""
	if _, err := s.UpdateProvider(provider.ID, ProviderUpdate{ProxyID: &direct}); err != nil {
		t.Fatalf("clear provider proxy: %v", err)
	}
	if err := s.DeleteProxy(proxy.ID); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if _, err := s.GetProxy(proxy.ID); err == nil {
		t.Fatal("expected proxy to be deleted")
	}
}

func TestProviderDefaultsToDirect(t *testing.T) {
	s := openTestStore(t)
	provider, err := s.CreateProvider(NewProvider{Name: "direct"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if provider.ProxyID != "" {
		t.Fatalf("proxy_id = %q, want direct", provider.ProxyID)
	}
}
