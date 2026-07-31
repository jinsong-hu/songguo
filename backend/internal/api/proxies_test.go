package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/store"
)

func TestProxyCRUDPasswordMaskAndDeleteConflict(t *testing.T) {
	s := newTestStore(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, http.MethodPost, "/api/proxies", "secret", strings.NewReader(
		`{"name":"office","type":"socks5","host":"127.0.0.1","port":1080,"username":"alice","password":"top-secret"}`,
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "top-secret") {
		t.Fatal("proxy response leaked the password")
	}
	var created proxyView
	decodeBody(t, rec, &created)
	if !created.HasPassword || created.Type != store.ProxyTypeSOCKS5 {
		t.Fatalf("created proxy = %+v", created)
	}

	rec = do(h, http.MethodGet, "/api/proxies", "secret", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "top-secret") {
		t.Fatalf("list: code = %d body = %s", rec.Code, rec.Body.String())
	}

	provider, err := s.CreateProvider(store.NewProvider{Name: "proxied", ProxyID: created.ID})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	rec = do(h, http.MethodDelete, "/api/proxies/"+created.ID, "secret", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete assigned: code = %d body = %s", rec.Code, rec.Body.String())
	}

	rec = do(h, http.MethodPatch, "/api/providers/"+provider.ID, "secret",
		strings.NewReader(`{"proxy_id":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("set provider direct: code = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodDelete, "/api/proxies/"+created.ID, "secret", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestProxyValidationAndProviderReference(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})

	rec := do(h, http.MethodPost, "/api/proxies", "secret",
		strings.NewReader(`{"name":"bad","type":"http","host":"proxy:12","port":0}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy: code = %d body = %s", rec.Code, rec.Body.String())
	}

	rec = do(h, http.MethodPatch, "/api/providers/missing", "secret",
		strings.NewReader(`{"proxy_id":"no-such-proxy"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider proxy: code = %d body = %s", rec.Code, rec.Body.String())
	}
}

// The proxy test dials whatever the proxy is actually for, so its target moves
// from the default probe to an assigned provider's origin. Port 1 is closed, so
// both cases fail to connect without leaving the machine — the assertion is on
// the target, which is chosen before any dialing.
func TestProxyTestTargetFollowsAssignedProvider(t *testing.T) {
	s := newTestStore(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	proxy, err := s.CreateProxy(store.NewProxy{
		Name: "dead", Type: store.ProxyTypeHTTPS, Host: "127.0.0.1", Port: 1,
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}

	rec := do(h, http.MethodPost, "/api/proxies/"+proxy.ID+"/test", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test unassigned: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var view testProxyView
	decodeBody(t, rec, &view)
	if view.Target != defaultProxyProbe {
		t.Fatalf("unassigned target = %q, want %q", view.Target, defaultProxyProbe)
	}
	if view.Reachable {
		t.Fatalf("closed port reported reachable: %+v", view)
	}

	if _, err := s.CreateProvider(store.NewProvider{
		Name:    "proxied",
		ProxyID: proxy.ID,
		Endpoints: []store.ProviderEndpoint{
			{Wire: "anthropic/messages", Endpoint: "https://api.anthropic.example/v1/messages"},
		},
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	rec = do(h, http.MethodPost, "/api/proxies/"+proxy.ID+"/test", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test assigned: code = %d body = %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &view)
	if view.Target != "https://api.anthropic.example" {
		t.Fatalf("assigned target = %q, want the provider origin", view.Target)
	}

	rec = do(h, http.MethodPost, "/api/proxies/missing/test", "secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown proxy: code = %d body = %s", rec.Code, rec.Body.String())
	}
}
