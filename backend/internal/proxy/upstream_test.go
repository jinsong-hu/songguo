package proxy

import (
	"testing"

	"github.com/songguo/songguo/internal/config"
)

// TestBuildUpstreamURL covers the model-routed URL construction: {model}
// substitution and query merging (an endpoint may carry its own query, e.g.
// Azure's api-version, which is unioned with any inbound query).
func TestBuildUpstreamURL(t *testing.T) {
	cases := []struct {
		name, endpoint, model, inboundQuery, want string
	}{
		{
			name:     "plain endpoint, no query",
			endpoint: "https://api.openai.com/v1/chat/completions",
			model:    "gpt-4o",
			want:     "https://api.openai.com/v1/chat/completions",
		},
		{
			name:         "plain endpoint with inbound query",
			endpoint:     "https://api.openai.com/v1/chat/completions",
			model:        "gpt-4o",
			inboundQuery: "stream=true",
			want:         "https://api.openai.com/v1/chat/completions?stream=true",
		},
		{
			name:     "model placeholder substituted",
			endpoint: "https://r.openai.azure.com/openai/deployments/{model}/chat/completions",
			model:    "gpt-4o",
			want:     "https://r.openai.azure.com/openai/deployments/gpt-4o/chat/completions",
		},
		{
			name:     "endpoint query preserved when no inbound query",
			endpoint: "https://r.openai.azure.com/openai/deployments/{model}/chat/completions?api-version=2024-10-21",
			model:    "gpt-4o",
			want:     "https://r.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21",
		},
		{
			name:         "endpoint query merged with inbound query",
			endpoint:     "https://r.openai.azure.com/openai/deployments/{model}/chat/completions?api-version=2024-10-21",
			model:        "gpt-4o",
			inboundQuery: "stream=true",
			want:         "https://r.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21&stream=true",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildUpstreamURL(c.endpoint, c.model, c.inboundQuery)
			if got != c.want {
				t.Errorf("buildUpstreamURL(%q, %q, %q) = %q, want %q", c.endpoint, c.model, c.inboundQuery, got, c.want)
			}
		})
	}
}

// TestPassthroughURL covers the allow_unmatched fallback: a child path under a
// known collection endpoint inherits that endpoint's rewritten base (so the
// video task-status GET reaches the same /api/plan/v3 base its submit was
// rewritten to), while an unrelated path falls back to the bare origin.
func TestPassthroughURL(t *testing.T) {
	ark := config.Vendor{
		Origin: "https://ark.cn-beijing.volces.com",
		Wires:  []string{"ark/video"},
		Endpoints: map[string]string{
			"ark/video": "https://ark.cn-beijing.volces.com/api/plan/v3/contents/generations/tasks",
		},
	}
	cases := []struct {
		name, inboundPath, rawQuery, want string
		vendor                            config.Vendor
	}{
		{
			name:        "video task-status child inherits rewritten base",
			vendor:      ark,
			inboundPath: "/api/v3/contents/generations/tasks/cgt-123",
			want:        "https://ark.cn-beijing.volces.com/api/plan/v3/contents/generations/tasks/cgt-123",
		},
		{
			name:        "video task-list child keeps inbound query",
			vendor:      ark,
			inboundPath: "/api/v3/contents/generations/tasks/cgt-123",
			rawQuery:    "verbose=true",
			want:        "https://ark.cn-beijing.volces.com/api/plan/v3/contents/generations/tasks/cgt-123?verbose=true",
		},
		{
			name:        "unrelated path falls back to bare origin",
			vendor:      ark,
			inboundPath: "/api/v3/some/other/path",
			want:        "https://ark.cn-beijing.volces.com/api/v3/some/other/path",
		},
		{
			name:        "bare collection path (no child tail) is not stem-rewritten",
			vendor:      ark,
			inboundPath: "/api/v3/contents/generations/tasks",
			want:        "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := passthroughURL(c.vendor, c.inboundPath, c.rawQuery)
			if got != c.want {
				t.Errorf("passthroughURL(%q, %q) = %q, want %q", c.inboundPath, c.rawQuery, got, c.want)
			}
		})
	}
}
