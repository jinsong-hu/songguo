package calls

import "testing"

func TestClassifyEntrypoint(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		want Entrypoint
	}{
		{
			name: "count_tokens by path",
			path: "/v1/messages/count_tokens",
			body: `{"model":"claude-opus-4-8","messages":[]}`,
			want: EntrypointCountTokens,
		},
		{
			name: "count_tokens with trailing slash",
			path: "/v1/messages/count_tokens/",
			body: ``,
			want: EntrypointCountTokens,
		},
		{
			name: "monitor by </block> stop sequence",
			path: "/v1/messages",
			body: `{"model":"claude-opus-4-8","max_tokens":64,"stop_sequences":["</block>"]}`,
			want: EntrypointMonitor,
		},
		{
			name: "utility by cc_workload in system blocks",
			path: "/v1/messages",
			body: `{"model":"claude-opus-4-8","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.211; cc_entrypoint=sdk-cli; cc_workload=compact;"}],"messages":[]}`,
			want: EntrypointUtility,
		},
		{
			name: "utility by cc_workload in system string",
			path: "/v1/messages",
			body: `{"system":"foo cc_entrypoint=sdk-cli; cc_workload=title; bar"}`,
			want: EntrypointUtility,
		},
		{
			name: "sdk-cli launch without cc_workload stays main",
			path: "/v1/messages",
			body: `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.211; cc_entrypoint=sdk-cli;"}]}`,
			want: EntrypointMain,
		},
		{
			name: "interactive cc_entrypoint=cli stays main",
			path: "/v1/messages",
			body: `{"system":[{"type":"text","text":"cc_entrypoint=cli;"}]}`,
			want: EntrypointMain,
		},
		{
			name: "ordinary main turn",
			path: "/v1/messages",
			body: `{"model":"claude-opus-4-8","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			want: EntrypointMain,
		},
		{
			name: "empty body is main",
			path: "/v1/chat/completions",
			body: ``,
			want: EntrypointMain,
		},
		{
			name: "unparseable body is main",
			path: "/v1/messages",
			body: `not json`,
			want: EntrypointMain,
		},
		{
			name: "unrelated stop sequence stays main",
			path: "/v1/messages",
			body: `{"stop_sequences":["\n\nHuman:"]}`,
			want: EntrypointMain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyEntrypoint(tc.path, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("ClassifyEntrypoint(%q, %q) = %q, want %q", tc.path, tc.body, got, tc.want)
			}
		})
	}
}

func TestEntrypointIsUtility(t *testing.T) {
	for ep, want := range map[Entrypoint]bool{
		"":                    false, // legacy row ⇒ main
		EntrypointMain:        false,
		EntrypointCountTokens: true,
		EntrypointMonitor:     true,
		EntrypointUtility:     true,
	} {
		if got := ep.IsUtility(); got != want {
			t.Errorf("Entrypoint(%q).IsUtility() = %v, want %v", ep, got, want)
		}
	}
}
