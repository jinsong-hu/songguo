package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPISpecParses checks the embedded spec is valid YAML, names an OpenAPI
// version, and was re-encoded to JSON at init.
func TestOpenAPISpecParses(t *testing.T) {
	if len(openapiYAML) == 0 {
		t.Fatal("embedded openapi.yaml is empty")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if _, ok := doc["openapi"]; !ok {
		t.Error("openapi.yaml is missing the top-level openapi version")
	}
	if len(openapiJSON) == 0 {
		t.Error("openapiJSON was not computed at init")
	}
}

// TestOpenAPIMatchesRoutes is the drift guard: every registered admin route must
// be documented in the spec, and the spec must document nothing else.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	httpMethods := map[string]bool{"get": true, "post": true, "patch": true, "delete": true, "put": true}

	specOps := map[string]bool{}
	for path, item := range doc.Paths {
		for method := range item {
			if httpMethods[method] {
				specOps[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	routeOps := map[string]bool{}
	for _, rt := range adminRoutes {
		key := rt.Method + " " + rt.Pattern
		routeOps[key] = true
		if !specOps[key] {
			t.Errorf("route %q is not documented in openapi.yaml", key)
		}
	}
	for op := range specOps {
		if !routeOps[op] {
			t.Errorf("openapi.yaml documents %q but no such route is registered", op)
		}
	}
}

// TestOpenAPIHandlerServes checks both representations are served with sane
// content types and that the JSON form is real JSON.
func TestOpenAPIHandlerServes(t *testing.T) {
	h := NewOpenAPIHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("yaml: code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("yaml content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "openapi:") {
		t.Error("yaml body does not look like an OpenAPI doc")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("json: code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("json content-type = %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("json body is not valid JSON: %v", err)
	}
	if _, ok := doc["openapi"]; !ok {
		t.Error("json body missing openapi version")
	}
}
