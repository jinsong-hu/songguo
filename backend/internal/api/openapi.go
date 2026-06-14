package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openapiYAML []byte

// openapiJSON is the embedded spec re-encoded as JSON at startup, so /openapi.json
// serves a real JSON document. Empty if the spec fails to parse (it is covered by
// a test, so that should never happen in a built binary).
var openapiJSON []byte

func init() {
	var doc any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		return
	}
	if b, err := json.Marshal(doc); err == nil {
		openapiJSON = b
	}
}

// NewOpenAPIHandler serves the embedded OpenAPI 3.1 spec for the /api admin
// surface. It is intentionally unauthenticated — the spec describes shapes only
// and contains no secrets — so tooling and agents can fetch the contract. A path
// ending in ".json" gets JSON; anything else gets the YAML source.
func NewOpenAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(openapiJSON)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiYAML)
	})
}
