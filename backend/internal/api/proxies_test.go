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
