package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	mcpkg "github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPClient struct {
	ID          string
	URL         string
	Transport   string
	Description string
	Setup       []string
	Tools       []mcpToolMeta
}

type mcpToolMeta struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

type MCPRegistry struct {
	mu       sync.RWMutex
	clients  []MCPClient
	sessions map[string]*mcpkg.ClientSession
	http     *http.Client
}

func NewMCPRegistry() *MCPRegistry {
	return &MCPRegistry{
		sessions: make(map[string]*mcpkg.ClientSession),
		http:     &http.Client{},
	}
}

func (r *MCPRegistry) Init(servers []map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeAllSessionsLocked()
	r.clients = nil
	if len(servers) == 0 {
		return nil
	}
	for _, m := range servers {
		if m == nil {
			continue
		}
		id, _ := m["id"].(string)
		url, _ := m["url"].(string)
		transport, _ := m["transport"].(string)
		desc, _ := m["description"].(string)

		var setup []string
		if rawSetup, ok := m["setup"].([]interface{}); ok {
			for _, v := range rawSetup {
				if sVal, ok := v.(string); ok {
					setup = append(setup, sVal)
				}
			}
		}

		id = strings.TrimSpace(id)
		url = strings.TrimSpace(url)
		transport = strings.TrimSpace(strings.ToLower(transport))
		if id == "" {
			continue
		}
		if url == "" && transport != "stdio" {
			continue
		}

		r.clients = append(r.clients, MCPClient{
			ID:          id,
			URL:         url,
			Transport:   transport,
			Description: strings.TrimSpace(desc),
			Setup:       setup,
		})
	}
	return nil
}

func LoadAllMCPProviders(registry *MCPRegistry, servers []map[string]interface{}) map[string]error {
	if registry == nil || len(servers) == 0 {
		return nil
	}
	if err := registry.Init(servers); err != nil {
		return map[string]error{"mcp": err}
	}
	if err := registry.LoadTools(); err != nil {
		return map[string]error{"mcp": err}
	}
	return nil
}

func (r *MCPRegistry) Cleanup() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeAllSessionsLocked()
	r.clients = nil
	log.Printf("tools-mcp: cleaned up")
	return nil
}

func (r *MCPRegistry) getClients() []MCPClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MCPClient, len(r.clients))
	copy(out, r.clients)
	return out
}

const defaultConnectTimeout = 15 * time.Second
const defaultListTimeout = 10 * time.Second
const defaultCallTimeout = 60 * time.Second

func (r *MCPRegistry) LoadTools() error {
	clients := r.getClients()
	if len(clients) == 0 {
		return nil
	}
	for i := range clients {
		ent := &clients[i]
		ent.Tools = nil
		session, err := r.getOrCreateSession(context.Background(), ent.ID)
		if err != nil {
			log.Printf("tools-mcp: connect to %s (%s) failed: %v", ent.ID, ent.URL, err)
			continue
		}
		listCtx, listCancel := context.WithTimeout(context.Background(), defaultListTimeout)
		res, err := session.ListTools(listCtx, nil)
		listCancel()
		if err != nil {
			r.dropSession(ent.ID)
			log.Printf("tools-mcp: list tools %s failed: %v", ent.ID, err)
			continue
		}
		for _, t := range res.Tools {
			ent.Tools = append(ent.Tools, mcpToolMeta{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  inputSchemaToParameters(&t.InputSchema),
			})
		}
	}
	r.mu.Lock()
	r.clients = clients
	r.mu.Unlock()
	return nil
}

func (r *MCPRegistry) GetAllTools() []map[string]interface{} {
	clients := r.getClients()
	var out []map[string]interface{}
	for _, ent := range clients {
		for _, tm := range ent.Tools {
			name := toolName(ent.ID, tm.Name)
			params := tm.Parameters
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			out = append(out, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": tm.Description,
					"parameters":  params,
				},
			})
		}
		metaName := toolName(ent.ID, "_meta")
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        metaName,
				"description": fmt.Sprintf("Get MCP server %s metadata, including description and setup instructions. This works even if the MCP server is offline.", ent.ID),
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		})
	}
	out = append(out, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "mcp_list_servers",
			"description": "返回所有已配置的 MCP server 及其工具列表（id, url, transport, description, setup, tools）。",
			"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
	})
	return out
}

func (r *MCPRegistry) ExecuteTool(ctx context.Context, name string, argumentsJSON string) (interface{}, error) {
	var params map[string]interface{}
	if argumentsJSON != "" {
		if err := json.Unmarshal([]byte(argumentsJSON), &params); err != nil {
			return nil, fmt.Errorf("mcp: invalid tool arguments: %w", err)
		}
	}
	if params == nil {
		params = make(map[string]interface{})
	}

	if name == "mcp_list_servers" {
		return r.executeListServers(), nil
	}
	if strings.HasPrefix(name, "mcp_") {
		serverID, toolNameVal, ok := r.parseQualifiedToolName(name)
		if ok {
			if toolNameVal == "_meta" {
				return r.getServerMeta(serverID), nil
			}
			return r.callTool(ctx, serverID, toolNameVal, params)
		}
	}
	return nil, fmt.Errorf("mcp: tool %q not found", name)
}

func (r *MCPRegistry) parseQualifiedToolName(name string) (serverID string, toolName string, ok bool) {
	if !strings.HasPrefix(name, "mcp_") {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, "mcp_")
	clients := r.getClients()
	if len(clients) == 0 {
		return "", "", false
	}
	var ids []string
	for _, c := range clients {
		if strings.TrimSpace(c.ID) != "" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return "", "", false
	}
	// prefer longest prefix, supports server IDs containing underscores
	sort.SliceStable(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	for _, id := range ids {
		prefix := id + "_"
		if strings.HasPrefix(rest, prefix) {
			tool := strings.TrimPrefix(rest, prefix)
			if tool == "" {
				return "", "", false
			}
			return id, tool, true
		}
	}
	return "", "", false
}

func (r *MCPRegistry) executeListServers() map[string]interface{} {
	clients := r.getClients()
	out := make([]map[string]interface{}, 0, len(clients))
	for _, s := range clients {
		tools := make([]map[string]interface{}, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
			})
		}
		out = append(out, map[string]interface{}{
			"id":          s.ID,
			"url":         s.URL,
			"transport":   s.Transport,
			"description": s.Description,
			"setup":       s.Setup,
			"tools":       tools,
			"toolsCount":  len(tools),
		})
	}
	return map[string]interface{}{
		"servers": out,
		"count":   len(out),
	}
}

func (r *MCPRegistry) getServerMeta(id string) map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.clients {
		if e.ID == id {
			return map[string]interface{}{
				"server_id":   e.ID,
				"url":         e.URL,
				"transport":   e.Transport,
				"description": e.Description,
				"setup":       e.Setup,
				"toolsCount":  len(e.Tools),
			}
		}
	}
	return map[string]interface{}{
		"error": fmt.Sprintf("mcp server %q not found", id),
	}
}

func toolName(serverID, name string) string {
	return "mcp_" + serverID + "_" + name
}

func (r *MCPRegistry) callTool(ctx context.Context, serverID, toolNameVal string, params map[string]interface{}) (interface{}, error) {
	session, err := r.getOrCreateSession(ctx, serverID)
	if err != nil {
		return nil, err
	}
	args := make(map[string]any)
	for k, v := range params {
		args[k] = v
	}
	callCtx, callCancel := context.WithTimeout(ctx, defaultCallTimeout)
	res, err := session.CallTool(callCtx, &mcpkg.CallToolParams{Name: toolNameVal, Arguments: args})
	callCancel()
	if err != nil {
		if errors.Is(err, mcpkg.ErrConnectionClosed) {
			r.dropSession(serverID)
			session, reconnectErr := r.getOrCreateSession(ctx, serverID)
			if reconnectErr == nil {
				callCtx2, cancel2 := context.WithTimeout(ctx, defaultCallTimeout)
				res, err = session.CallTool(callCtx2, &mcpkg.CallToolParams{Name: toolNameVal, Arguments: args})
				cancel2()
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("mcp call_tool %s/%s: %w", serverID, toolNameVal, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp tool error: %s", res.Content)
	}
	return contentToInterface(res.Content), nil
}

func contentToInterface(content []mcpkg.Content) interface{} {
	if len(content) == 0 {
		return map[string]interface{}{"result": ""}
	}
	var parts []string
	var out []map[string]interface{}
	for _, c := range content {
		if t, ok := c.(*mcpkg.TextContent); ok {
			parts = append(parts, t.Text)
			out = append(out, map[string]interface{}{"type": "text", "text": t.Text})
		} else {
			parts = append(parts, fmt.Sprintf("%v", c))
			out = append(out, map[string]interface{}{"type": "unknown", "raw": fmt.Sprintf("%v", c)})
		}
	}
	if len(parts) == 1 {
		return map[string]interface{}{"result": parts[0]}
	}
	return map[string]interface{}{"result": strings.Join(parts, "\n"), "parts": out}
}

func inputSchemaToParameters(schema interface{}) map[string]interface{} {
	if schema == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	var m map[string]interface{}
	b, _ := json.Marshal(schema)
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if _, has := m["type"]; !has {
		m["type"] = "object"
	}
	if _, has := m["properties"]; !has {
		m["properties"] = map[string]interface{}{}
	}
	return m
}

func (r *MCPRegistry) getClientByID(serverID string) (MCPClient, bool) {
	for _, c := range r.getClients() {
		if c.ID == serverID {
			return c, true
		}
	}
	return MCPClient{}, false
}

func (r *MCPRegistry) getOrCreateSession(ctx context.Context, serverID string) (*mcpkg.ClientSession, error) {
	r.mu.RLock()
	if s := r.sessions[serverID]; s != nil {
		r.mu.RUnlock()
		return s, nil
	}
	r.mu.RUnlock()

	clientCfg, ok := r.getClientByID(serverID)
	if !ok {
		return nil, fmt.Errorf("mcp server %q not found", serverID)
	}
	transport, err := r.newTransport(clientCfg)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()
	client := mcpkg.NewClient(&mcpkg.Implementation{Name: "octosucker-mcp", Version: "1.0.0"}, nil)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect to %s: %w", serverID, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.sessions[serverID]; existing != nil {
		_ = session.Close()
		return existing, nil
	}
	r.sessions[serverID] = session
	return session, nil
}

func (r *MCPRegistry) newTransport(clientCfg MCPClient) (mcpkg.Transport, error) {
	switch clientCfg.Transport {
	case "sse":
		return &mcpkg.SSEClientTransport{Endpoint: clientCfg.URL, HTTPClient: r.http}, nil
	case "stdio":
		return nil, fmt.Errorf("mcp server %q uses stdio transport which is no longer supported", clientCfg.ID)
	default:
		return &mcpkg.StreamableClientTransport{Endpoint: clientCfg.URL, HTTPClient: r.http}, nil
	}
}

func (r *MCPRegistry) dropSession(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[serverID]; s != nil {
		_ = s.Close()
		delete(r.sessions, serverID)
	}
}

func (r *MCPRegistry) closeAllSessionsLocked() {
	for id, s := range r.sessions {
		if s != nil {
			_ = s.Close()
		}
		delete(r.sessions, id)
	}
}
