package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// defaultProxyProbe is dialled by the proxy test when no provider is assigned
// yet. An LLM vendor origin rather than a generic site: these proxies exist to
// reach vendors, so a tunnel that only carries other traffic is not a pass.
const defaultProxyProbe = "https://api.openai.com"

type proxyView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	HasPassword   bool   `json:"has_password"`
	ProviderCount int    `json:"provider_count"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type createProxyReq struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type patchProxyReq struct {
	Name          *string `json:"name,omitempty"`
	Type          *string `json:"type,omitempty"`
	Host          *string `json:"host,omitempty"`
	Port          *int    `json:"port,omitempty"`
	Username      *string `json:"username,omitempty"`
	Password      *string `json:"password,omitempty"`
	ClearPassword bool    `json:"clear_password,omitempty"`
}

func newProxyView(p store.Proxy) proxyView {
	return proxyView{
		ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port,
		Username: p.Username, HasPassword: p.Password != "",
		ProviderCount: p.ProviderCount,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (a *api) handleListProxies(w http.ResponseWriter, _ *http.Request) {
	views, err := a.proxiesData()
	if err != nil {
		a.writeDataErr(w, "list proxies", err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *api) proxiesData() ([]proxyView, error) {
	proxies, err := a.store.ListProxies()
	if err != nil {
		return nil, err
	}
	views := make([]proxyView, 0, len(proxies))
	for _, p := range proxies {
		views = append(views, newProxyView(p))
	}
	return views, nil
}

// handleTestProxy probes an external origin through the proxy to prove the
// tunnel works end to end.
func (a *api) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	view, err := a.testProxyData(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get proxy", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// testProxyData dials the origin of a provider assigned to this proxy, falling
// back to defaultProxyProbe when none is assigned — a freshly added proxy has
// no providers yet, and that is exactly when it needs testing. No credential is
// sent: any HTTP status proves the tunnel carried the request, which is the
// only thing this test claims. Reachability failures are reported in the view
// (not as errors); only an unknown id returns a *apiError (404).
func (a *api) testProxyData(ctx context.Context, id string) (testProxyView, error) {
	p, err := a.store.GetProxy(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return testProxyView{}, notFoundErr("proxy not found")
		}
		return testProxyView{}, err
	}

	origin, err := a.proxyProbeOrigin(id)
	if err != nil {
		return testProxyView{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		return testProxyView{Target: origin, Reachable: false, Error: err.Error()}, nil
	}

	start := a.now()
	resp, err := a.outbound.Do(req, &config.Proxy{
		ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port,
		Username: p.Username, Password: p.Password,
	})
	latency := a.now().Sub(start).Milliseconds()
	if err != nil {
		return testProxyView{Target: origin, Reachable: false, LatencyMS: latency, Error: err.Error()}, nil
	}
	defer resp.Body.Close()
	drain(resp.Body)

	return testProxyView{Target: origin, Reachable: true, Status: resp.StatusCode, LatencyMS: latency}, nil
}

// proxyProbeOrigin returns the origin to dial through a proxy: the first usable
// endpoint of a provider assigned to it, or defaultProxyProbe when the proxy
// has no assigned provider with a parseable endpoint.
func (a *api) proxyProbeOrigin(proxyID string) (string, error) {
	providers, err := a.store.ListProviders()
	if err != nil {
		return "", err
	}
	for _, pvd := range providers {
		if pvd.ProxyID != proxyID {
			continue
		}
		for _, ep := range pvd.Endpoints {
			if origin, err := originOf(ep.Endpoint); err == nil {
				return origin, nil
			}
		}
	}
	return defaultProxyProbe, nil
}

func (a *api) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var req createProxyReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.createProxyData(req)
	if err != nil {
		a.writeDataErr(w, "create proxy", err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *api) createProxyData(req createProxyReq) (proxyView, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.Host = normalizeProxyHost(req.Host)
	if msg := validateProxyFields(req.Name, req.Type, req.Host, req.Port); msg != "" {
		return proxyView{}, badRequestErr(msg)
	}
	p, err := a.store.CreateProxy(store.NewProxy{
		Name: req.Name, Type: req.Type, Host: req.Host, Port: req.Port,
		Username: req.Username, Password: req.Password,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return proxyView{}, conflictErr("a proxy with that name already exists")
		}
		return proxyView{}, err
	}
	a.reloadAfterWrite()
	return newProxyView(p), nil
}

func (a *api) handlePatchProxy(w http.ResponseWriter, r *http.Request) {
	var req patchProxyReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.updateProxyData(r.PathValue("id"), req)
	if err != nil {
		a.writeDataErr(w, "update proxy", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *api) updateProxyData(id string, req patchProxyReq) (proxyView, error) {
	current, err := a.store.GetProxy(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return proxyView{}, notFoundErr("proxy not found")
		}
		return proxyView{}, err
	}
	if req.Password != nil && req.ClearPassword {
		return proxyView{}, badRequestErr("password and clear_password cannot both be set")
	}

	name, proxyType, host, port := current.Name, current.Type, current.Host, current.Port
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		req.Name = &name
	}
	if req.Type != nil {
		proxyType = strings.ToLower(strings.TrimSpace(*req.Type))
		req.Type = &proxyType
	}
	if req.Host != nil {
		host = normalizeProxyHost(*req.Host)
		req.Host = &host
	}
	if req.Port != nil {
		port = *req.Port
	}
	if msg := validateProxyFields(name, proxyType, host, port); msg != "" {
		return proxyView{}, badRequestErr(msg)
	}
	if req.ClearPassword {
		empty := ""
		req.Password = &empty
	}

	p, err := a.store.UpdateProxy(id, store.ProxyUpdate{
		Name: req.Name, Type: req.Type, Host: req.Host, Port: req.Port,
		Username: req.Username, Password: req.Password,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return proxyView{}, notFoundErr("proxy not found")
		}
		if isUniqueViolation(err) {
			return proxyView{}, conflictErr("a proxy with that name already exists")
		}
		return proxyView{}, err
	}
	a.reloadAfterWrite()
	return newProxyView(p), nil
}

func (a *api) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if err := a.deleteProxyData(r.PathValue("id")); err != nil {
		a.writeDataErr(w, "delete proxy", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) deleteProxyData(id string) error {
	p, err := a.store.GetProxy(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundErr("proxy not found")
		}
		return err
	}
	if p.ProviderCount > 0 {
		return conflictErr("proxy is assigned to one or more providers; set them to Direct before deleting it")
	}
	if err := a.store.DeleteProxy(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundErr("proxy not found")
		}
		return err
	}
	a.reloadAfterWrite()
	return nil
}

func normalizeProxyHost(host string) string {
	return strings.Trim(strings.TrimSpace(host), "[]")
}

func validateProxyFields(name, proxyType, host string, port int) string {
	if name == "" {
		return "name is required"
	}
	if proxyType != store.ProxyTypeHTTPS && proxyType != store.ProxyTypeSOCKS5 {
		return "type must be https or socks5"
	}
	if host == "" {
		return "host is required"
	}
	if strings.ContainsAny(host, "/?#@") || strings.Contains(host, "://") {
		return "host must not include a scheme, credentials, port, or path"
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "host must not include a port"
	}
	if port < 1 || port > 65535 {
		return "port must be between 1 and 65535"
	}
	return ""
}
