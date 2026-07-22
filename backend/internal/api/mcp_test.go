package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpReadTools / mcpWriteTools are the expected tool names, mirrored from
// buildMCPServer. Kept here so the test fails loudly if the catalogue changes.
var mcpReadTools = []string{
	"get_overview", "get_usage_series", "list_calls", "get_call_trace",
	"list_users", "list_providers", "list_services", "list_pricing", "get_settings",
}

var mcpWriteTools = []string{
	"create_user", "update_user", "revoke_user", "create_provider",
	"update_provider", "delete_provider", "test_provider",
}

// connectMCP wires an in-memory client to a server built from a, returning the
// connected client session.
func connectMCP(t *testing.T, a *api, enableWrites bool) *mcp.ClientSession {
	t.Helper()
	srv := a.buildMCPServer(enableWrites)
	ctx := context.Background()

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	lt, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range lt.Tools {
		names[tool.Name] = true
	}
	return names
}

// contentText concatenates the text content blocks of a tool result, for
// readable assertion failures.
func contentText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMCPReadOnlyByDefault verifies read tools are exposed, write tools are not,
// and a couple of read tools execute without error.
func TestMCPReadOnlyByDefault(t *testing.T) {
	s := newTestStore(t)
	a := newAPI(Deps{Store: s, AdminKey: "secret"})
	cs := connectMCP(t, a, false)

	names := toolNames(t, cs)
	for _, want := range mcpReadTools {
		if !names[want] {
			t.Errorf("read tool %q missing from tools/list", want)
		}
	}
	for _, write := range mcpWriteTools {
		if names[write] {
			t.Errorf("write tool %q exposed but writes are disabled", write)
		}
	}

	for _, name := range []string{"get_overview", "list_users", "list_providers", "get_settings"} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      name,
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		if res.IsError {
			t.Errorf("%s returned a tool error: %s", name, contentText(res.Content))
		}
	}
}

// TestMCPWriteToolsWhenEnabled verifies write tools appear only with the opt-in
// and that a create round-trips.
func TestMCPWriteToolsWhenEnabled(t *testing.T) {
	s := newTestStore(t)
	a := newAPI(Deps{Store: s, AdminKey: "secret"})
	cs := connectMCP(t, a, true)

	names := toolNames(t, cs)
	for _, want := range mcpWriteTools {
		if !names[want] {
			t.Errorf("write tool %q missing when writes are enabled", want)
		}
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_user",
		Arguments: map[string]any{"name": "agent"},
	})
	if err != nil {
		t.Fatalf("call create_user: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_user returned a tool error: %s", contentText(res.Content))
	}
}

// TestMCPHandlerRequiresAdminKey verifies the HTTP-mounted MCP endpoint is gated
// by the admin bearer key, like the REST API.
func TestMCPHandlerRequiresAdminKey(t *testing.T) {
	h := NewMCPHandler(Deps{AdminKey: "secret"}, false)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	req := httptest.NewRequest(http.MethodPost, "/admin/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: code = %d, want 401", rec.Code)
	}
}
