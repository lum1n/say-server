package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lum1n/say-server/internal/llm"
	"github.com/lum1n/say-server/internal/store"
)

// An agent's tools come from the MCP servers its admin gave it. It opens
// them for each reply and closes them after, so no session outlives the
// node that holds the agent.

var (
	// maxToolRounds is how many times one reply may go back to the model
	// with tool results.
	maxToolRounds = 8
	// connectTimeout bounds opening one server and listing its tools.
	connectTimeout = 15 * time.Second
	// toolTimeout bounds one tool call.
	toolTimeout = time.Minute
)

// maxToolResult is the most of a tool's answer the model sees.
const maxToolResult = 16 << 10

// toolbox is the tools an agent has for one reply.
type toolbox struct {
	tools    []llm.Tool
	routes   map[string]toolRoute // by the name the model sees
	sessions []*mcp.ClientSession
}

type toolRoute struct {
	session *mcp.ClientSession
	name    string // the server's name for the tool
}

// openTools connects to servers at once and lists their tools. A server
// that fails is left out, and its error returned with the others, so the
// agent still answers without it.
func openTools(ctx context.Context, version string, servers []store.MCPServer) (*toolbox, error) {
	type opened struct {
		session *mcp.ClientSession
		tools   []*mcp.Tool
		err     error
	}
	results := make([]opened, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Go(func() {
			s, tools, err := openServer(ctx, version, srv)
			if err != nil {
				err = fmt.Errorf("tools from %q: %w", srv.Name, err)
			}
			results[i] = opened{s, tools, err}
		})
	}
	wg.Wait()
	t := &toolbox{routes: map[string]toolRoute{}}
	var errs []error
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		t.sessions = append(t.sessions, r.session)
		for _, tool := range r.tools {
			name := toolName(servers[i].Name, tool.Name)
			if _, dup := t.routes[name]; dup {
				continue
			}
			t.routes[name] = toolRoute{session: r.session, name: tool.Name}
			desc := tool.Description
			if desc == "" {
				desc = tool.Title
			}
			t.tools = append(t.tools, llm.Tool{Name: name, Description: desc, InputSchema: inputSchema(tool.InputSchema)})
		}
	}
	return t, errors.Join(errs...)
}

func openServer(ctx context.Context, version string, srv store.MCPServer) (*mcp.ClientSession, []*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	httpClient := http.DefaultClient
	if srv.Authorization != "" {
		httpClient = &http.Client{Transport: authTransport{auth: srv.Authorization, next: http.DefaultTransport}}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "say", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           httpClient,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			session.Close()
			return nil, nil, err
		}
		tools = append(tools, tool)
	}
	return session, tools, nil
}

// authTransport sends a server's Authorization header.
type authTransport struct {
	auth string
	next http.RoundTripper
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", t.auth)
	return t.next.RoundTrip(r)
}

var notToolChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolName is the name the model sees: the server's name and the tool's,
// in the characters and length both providers accept.
func toolName(server, tool string) string {
	name := notToolChar.ReplaceAllString(server+"__"+tool, "_")
	return name[:min(len(name), 64)]
}

// inputSchema turns a tool's JSON Schema into an object schema, which is
// what providers take.
func inputSchema(v any) map[string]any {
	var schema map[string]any
	if b, err := json.Marshal(v); err == nil {
		json.Unmarshal(b, &schema)
	}
	if schema == nil {
		schema = map[string]any{}
	}
	schema["type"] = "object"
	return schema
}

func (t *toolbox) list() []llm.Tool {
	if t == nil {
		return nil
	}
	return t.tools
}

// call runs one tool call. Failures go back to the model as error results.
func (t *toolbox) call(ctx context.Context, c llm.ToolCall) llm.ToolResult {
	fail := func(format string, args ...any) llm.ToolResult {
		return llm.ToolResult{CallID: c.ID, Content: fmt.Sprintf(format, args...), IsError: true}
	}
	var route toolRoute
	ok := false
	if t != nil {
		route, ok = t.routes[c.Name]
	}
	if !ok {
		return fail("there is no tool called %q", c.Name)
	}
	input := c.Input
	if len(strings.TrimSpace(string(input))) == 0 {
		input = json.RawMessage("{}")
	}
	if !json.Valid(input) {
		return fail("the arguments are not valid JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	res, err := route.session.CallTool(ctx, &mcp.CallToolParams{Name: route.name, Arguments: input})
	if err != nil {
		return fail("%v", err)
	}
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		case *mcp.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			} else {
				parts = append(parts, "[resource]")
			}
		case *mcp.ImageContent:
			parts = append(parts, "[image]")
		case *mcp.AudioContent:
			parts = append(parts, "[audio]")
		case *mcp.ResourceLink:
			parts = append(parts, c.URI)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(b))
		}
	}
	text := strings.Join(parts, "\n")
	if len(text) > maxToolResult {
		text = strings.ToValidUTF8(text[:maxToolResult], "") + "\n[cut: the result was longer]"
	}
	return llm.ToolResult{CallID: c.ID, Content: text, IsError: res.IsError}
}

func (t *toolbox) close() {
	if t == nil {
		return
	}
	for _, s := range t.sessions {
		s.Close()
	}
}
