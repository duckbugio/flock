// Package knowledge exposes only authorized read-only knowledge operations to delegated agents.
package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxBody          = 2 << 20
	operationTimeout = 30 * time.Second
	headerTimeout    = 5 * time.Second
	writeTimeout     = operationTimeout + headerTimeout
)

var leaves = [...]string{"kb_search", "kb_get", "kb_list"}

// Proxy keeps the upstream credential inside the trusted host process.
type Proxy struct {
	URL   string
	Token string
	close func()
}

// Close releases the local listener and its upstream session.
func (p *Proxy) Close() { p.close() }

type bearerTransport struct {
	base   http.RoundTripper
	bearer string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	forwarded := req.Clone(req.Context())
	forwarded.Header = req.Header.Clone()
	forwarded.Header.Set("Authorization", t.bearer)
	response, err := t.base.RoundTrip(forwarded)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxBody {
		_ = response.Body.Close()
		return nil, errors.New("knowledge response too large")
	}
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: maxBody}
	return response, nil
}

// Bound streamed/chunked responses before the MCP decoder allocates their JSON.
type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errors.New("knowledge response too large")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// Start uses the bot's existing scope. It never adds or overrides a project ID.
func Start(ctx context.Context, endpoint, authorization string) (*Proxy, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) {
		return nil, errors.New("invalid knowledge endpoint")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "flock-secretary-knowledge", Version: "1"}, nil)
	httpClient := &http.Client{
		Transport: bearerTransport{base: http.DefaultTransport, bearer: authorization},
		Timeout:   operationTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, errors.New("knowledge connection failed")
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	server, err := readServer(discoveryCtx, session)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = session.Close()
		return nil, errors.New("knowledge proxy unavailable")
	}
	token := rand.Text()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBody,
			PropagateRequestCancellation: true,
		})
	protected := http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/mcp" || subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	host := &http.Server{
		Handler: protected, ReadHeaderTimeout: headerTimeout, ReadTimeout: operationTimeout,
		WriteTimeout: writeTimeout, IdleTimeout: time.Minute,
	}
	go func() { _ = host.Serve(listener) }()
	var once sync.Once
	return &Proxy{URL: "http://" + listener.Addr().String() + "/mcp", Token: token, close: func() {
		once.Do(func() {
			_ = host.Close()
			_ = session.Close()
		})
	}}, nil
}

type catalogTool struct {
	Name        string          `json:"name"`
	Mutates     *bool           `json:"mutates"`
	Execution   string          `json:"execution"`
	Surface     string          `json:"surface"`
	DirectOnly  bool            `json:"directOnly"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func readServer(ctx context.Context, session *mcp.ClientSession) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "flock-secretary-knowledge", Version: "1"}, nil)
	for _, leaf := range leaves {
		response, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "discover_tools",
			Arguments: map[string]any{"name": leaf, "includeSchema": true},
		})
		if err != nil {
			return nil, errors.New("knowledge discovery failed")
		}
		var discovery struct {
			Tools []catalogTool `json:"tools"`
		}
		if err := resultJSON(response, &discovery); err != nil {
			return nil, err
		}
		if len(discovery.Tools) == 0 {
			continue // The bot token does not authorize this operation.
		}
		if len(discovery.Tools) != 1 {
			return nil, errors.New("ambiguous knowledge tool")
		}
		tool := discovery.Tools[0]
		if tool.Name != leaf || tool.Mutates == nil || *tool.Mutates || tool.Execution != "read" ||
			tool.Surface != "catalog" || tool.DirectOnly {
			return nil, errors.New("knowledge tool is not an authorized catalog read")
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema.Type != "object" {
			return nil, errors.New("invalid knowledge tool schema")
		}
		resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
		if err != nil {
			return nil, errors.New("unsupported knowledge tool schema")
		}
		// Low-level registration deliberately uses the pre-resolved schema so a
		// malformed upstream schema cannot panic or fetch external references.
		server.AddTool(&mcp.Tool{
			Name: leaf, InputSchema: tool.InputSchema,
			Description: "Read the owner's authorized knowledge using " + leaf,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(callCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > maxBody || json.Unmarshal(req.Params.Arguments, &args) != nil ||
				args == nil || resolved.Validate(args) != nil {
				return nil, errors.New("invalid knowledge arguments")
			}
			callCtx, cancel := context.WithTimeout(callCtx, operationTimeout)
			defer cancel()
			result, err := session.CallTool(callCtx, &mcp.CallToolParams{
				Name:      "execute_tool",
				Arguments: map[string]any{"name": leaf, "arguments": args},
			})
			if err != nil {
				return nil, errors.New("knowledge read failed")
			}
			var envelope struct {
				Name   string          `json:"name"`
				Result json.RawMessage `json:"result"`
			}
			if err := resultJSON(result, &envelope); err != nil || envelope.Name != leaf || len(envelope.Result) == 0 {
				return nil, errors.New("invalid knowledge read result")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(envelope.Result)}}}, nil
		})
	}
	return server, nil
}

func resultJSON(result *mcp.CallToolResult, target any) error {
	if result == nil || result.IsError {
		return errors.New("knowledge operation unavailable")
	}
	var data []byte
	var err error
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
	} else if len(result.Content) == 1 {
		if text, ok := result.Content[0].(*mcp.TextContent); ok {
			data = []byte(text.Text)
		}
	}
	if err != nil || len(data) == 0 || len(data) > maxBody || json.Unmarshal(data, target) != nil {
		return errors.New("invalid knowledge response")
	}
	return nil
}
