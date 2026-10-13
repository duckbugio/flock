//nolint:testpackage // whitebox validation of upstream metadata and the credential transport.
package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fakeKnowledge(t *testing.T, badMetadata bool) (string, *[]string, *sync.Mutex) {
	t.Helper()
	var calls []string
	var mu sync.Mutex
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	type discoveryInput struct {
		Name          string `json:"name"`
		IncludeSchema bool   `json:"includeSchema"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "discover_tools"},
		func(_ context.Context, _ *mcp.CallToolRequest, input discoveryInput) (*mcp.CallToolResult, any, error) {
			mutates := badMetadata
			return nil, map[string]any{"tools": []catalogTool{{
				Name: input.Name, Mutates: &mutates, Execution: "read",
				Surface: "catalog", InputSchema: json.RawMessage(`{"type":"object",` +
					`"properties":{"query":{"type":"string"},"projectId":{"type":"string"}},` +
					`"required":["query"],"additionalProperties":false}`),
			}}}, nil
		})
	type executeInput struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "execute_tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, input executeInput) (*mcp.CallToolResult, any, error) {
			mu.Lock()
			calls = append(calls, input.Name)
			mu.Unlock()
			return nil, map[string]any{"name": input.Name, "result": map[string]any{
				"answer":    "recipient-safe fixture",
				"projectId": input.Arguments["projectId"],
			}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer upstream-fixture" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer.URL, &calls, &mu
}

func proxyClient(t *testing.T, proxy *Proxy) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             proxy.URL,
		HTTPClient:           &http.Client{Transport: bearerTransport{base: http.DefaultTransport, bearer: "Bearer " + proxy.Token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestProxyOnlyReadsKnowledgeAndPreservesBotScope(t *testing.T) {
	endpoint, calls, mu := fakeKnowledge(t, false)
	proxy, err := Start(t.Context(), endpoint, "Bearer upstream-fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	client := proxyClient(t, proxy)
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil || len(listed.Tools) != 3 {
		t.Fatalf("tools = %v, error %v", listed, err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "kb_search" && tool.Name != "kb_get" && tool.Name != "kb_list" {
			t.Fatalf("unexpected tool %s", tool.Name)
		}
	}
	for _, name := range []string{"execute_tool", "execute_write_tool", "sendMessage", "kb_delete", "KB_SEARCH"} {
		_, err := proxyClient(t, proxy).CallTool(t.Context(), &mcp.CallToolParams{
			Name: name, Arguments: map[string]any{"query": "read"},
		})
		if err == nil {
			t.Fatalf("forbidden tool %s succeeded", name)
		}
	}
	for _, args := range []map[string]any{{}, {"query": 3}, {"query": "read", "name": "sendMessage"}} {
		_, err := proxyClient(t, proxy).CallTool(t.Context(), &mcp.CallToolParams{Name: "kb_search", Arguments: args})
		if err == nil {
			t.Fatal("invalid schema arguments succeeded")
		}
	}
	mu.Lock()
	if len(*calls) != 0 {
		t.Fatalf("forbidden requests reached upstream: %v", *calls)
	}
	mu.Unlock()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "kb_search",
		Arguments: map[string]any{"query": "fixture", "projectId": "owner-selected-project"},
	})
	if err != nil || result.IsError {
		t.Fatalf("authorized read failed: %v", err)
	}
	var answer map[string]any
	if err := resultJSON(result, &answer); err != nil || answer["projectId"] != "owner-selected-project" {
		t.Fatal("scope argument was changed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 || (*calls)[0] != "kb_search" {
		t.Fatalf("upstream calls = %v", *calls)
	}
}

func TestProxyRejectsMutatingMetadataAndUntrustedEndpoint(t *testing.T) {
	endpoint, calls, mu := fakeKnowledge(t, true)
	proxy, err := Start(t.Context(), endpoint, "Bearer upstream-fixture")
	if err == nil {
		proxy.Close()
		t.Fatal("mutating tool metadata admitted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 0 {
		t.Fatal("mutating tool reached executor")
	}
	for _, endpoint := range []string{"file:///secret", "http://example.com/mcp", "https://user:password@example.com/mcp"} {
		if _, err := Start(t.Context(), endpoint, "Bearer fixture"); err == nil {
			t.Fatalf("unsafe endpoint admitted: %s", endpoint)
		}
	}
}
