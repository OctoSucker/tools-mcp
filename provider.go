package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	tools "github.com/OctoSucker/octosucker-tools"
	mcpkg "github.com/modelcontextprotocol/go-sdk/mcp"
)

const providerName = "github.com/OctoSucker/tools-mcp"

type serverEntry struct {
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

type Provider struct {
	mu      sync.RWMutex
	servers []serverEntry
}

func (s *Provider) Init(config map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers = nil
	if config == nil {
		log.Printf("skill-mcp: no config, no MCP servers configured")
		return nil
	}
	raw, _ := config["servers"].([]interface{})
	for _, x := range raw {
		m, _ := x.(map[string]interface{})
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

		s.servers = append(s.servers, serverEntry{
			ID:          id,
			URL:         url,
			Transport:   transport,
			Description: strings.TrimSpace(desc),
			Setup:       setup,
		})
	}
	return nil
}

func (s *Provider) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers = nil
	log.Printf("skill-mcp: cleaned up")
	return nil
}

func (s *Provider) getServers() []serverEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]serverEntry, len(s.servers))
	copy(out, s.servers)
	return out
}

func (s *Provider) getServerMeta(id string) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.servers {
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

var globalProvider *Provider

func RegisterMCPSkill(registry *tools.ToolRegistry, agent interface{}) error {
	servers := globalProvider.getServers()
	if len(servers) == 0 {
		log.Printf("skill-mcp: no servers in config, skipping tool registration")
		return nil
	}
	client := mcpkg.NewClient(&mcpkg.Implementation{Name: "octosucker-mcp-skill", Version: "1.0.0"}, nil)
	httpClient := &http.Client{}
	for i := range servers {
		ent := &servers[i]
		var transport mcpkg.Transport
		switch ent.Transport {
		case "sse":
			transport = &mcpkg.SSEClientTransport{Endpoint: ent.URL, HTTPClient: httpClient}
		case "stdio":
			log.Printf("skill-mcp: server %s uses stdio transport which is no longer supported, skipping", ent.ID)
			continue
		default:
			transport = &mcpkg.StreamableClientTransport{Endpoint: ent.URL, HTTPClient: httpClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultConnectTimeout)
		session, err := client.Connect(ctx, transport, nil)
		cancel()
		if err != nil {
			log.Printf("skill-mcp: connect to %s (%s) failed: %v", ent.ID, ent.URL, err)
			continue
		}
		listCtx, listCancel := context.WithTimeout(context.Background(), defaultListTimeout)
		res, err := session.ListTools(listCtx, nil)
		listCancel()
		_ = session.Close()
		if err != nil {
			log.Printf("skill-mcp: list tools %s failed: %v", ent.ID, err)
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
	globalProvider.mu.Lock()
	globalProvider.servers = servers
	globalProvider.mu.Unlock()

	total := 0
	for _, ent := range servers {
		for _, tm := range ent.Tools {
			name := toolName(ent.ID, tm.Name)
			params := tm.Parameters
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			serverID, toolNameVal := ent.ID, tm.Name
			registry.Register(&tools.Tool{
				Name:        name,
				Description: tm.Description,
				Parameters:  params,
				Handler:     makeMCPHandler(serverID, toolNameVal),
			})
			total++
		}

		metaName := toolName(ent.ID, "_meta")
		registry.Register(&tools.Tool{
			Name:        metaName,
			Description: fmt.Sprintf("Get MCP server %s metadata, including description and setup instructions. This works even if the MCP server is offline.", ent.ID),
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			Handler:     makeMCPMetaHandler(ent.ID),
		})
		total++
	}

	// 额外注册一个聚合工具：返回所有已注册的 MCP server 及其工具列表
	registry.Register(&tools.Tool{
		Name:        "mcp_list_servers",
		Description: "返回所有已配置的 MCP server 及其工具列表（id, url, transport, description, setup, tools）。",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Handler: func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
			servers := globalProvider.getServers()
			out := make([]map[string]interface{}, 0, len(servers))
			for _, s := range servers {
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
			}, nil
		},
	})

	var parts []string
	for _, ent := range servers {
		if len(ent.Tools) > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", ent.ID, len(ent.Tools)))
		} else {
			parts = append(parts, fmt.Sprintf("%s:0(connect/list failed)", ent.ID))
		}
	}
	return nil
}

func toolName(serverID, name string) string {
	return "mcp_" + serverID + "_" + name
}

func makeMCPHandler(serverID, toolNameVal string) tools.ToolHandler {
	return func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
		return globalProvider.callTool(ctx, serverID, toolNameVal, params)
	}
}

func makeMCPMetaHandler(serverID string) tools.ToolHandler {
	return func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
		return globalProvider.getServerMeta(serverID), nil
	}
}

const defaultConnectTimeout = 15 * time.Second
const defaultListTimeout = 10 * time.Second
const defaultCallTimeout = 60 * time.Second

func (s *Provider) callTool(ctx context.Context, serverID, toolNameVal string, params map[string]interface{}) (interface{}, error) {
	var serverURL, serverTransport string
	for _, e := range s.getServers() {
		if e.ID == serverID {
			serverURL = e.URL
			serverTransport = e.Transport
			break
		}
	}
	if serverURL == "" {
		return nil, fmt.Errorf("mcp server %q not found", serverID)
	}
	client := mcpkg.NewClient(&mcpkg.Implementation{Name: "octosucker-mcp-skill", Version: "1.0.0"}, nil)
	httpClient := &http.Client{}
	var transport mcpkg.Transport
	switch serverTransport {
	case "sse":
		transport = &mcpkg.SSEClientTransport{Endpoint: serverURL, HTTPClient: httpClient}
	case "stdio":
		return nil, fmt.Errorf("mcp server %q uses stdio transport which is no longer supported", serverID)
	default:
		transport = &mcpkg.StreamableClientTransport{Endpoint: serverURL, HTTPClient: httpClient}
	}
	connectCtx, connectCancel := context.WithTimeout(ctx, defaultConnectTimeout)
	session, err := client.Connect(connectCtx, transport, nil)
	connectCancel()
	if err != nil {
		return nil, fmt.Errorf("mcp connect to %s: %w", serverID, err)
	}
	defer session.Close()
	args := make(map[string]any)
	for k, v := range params {
		args[k] = v
	}
	callCtx, callCancel := context.WithTimeout(ctx, defaultCallTimeout)
	res, err := session.CallTool(callCtx, &mcpkg.CallToolParams{Name: toolNameVal, Arguments: args})
	callCancel()
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

func init() {
	globalProvider = &Provider{}
	tools.RegisterToolProviderWithMetadata(providerName, tools.ToolProviderMetadata{
		Name:        providerName,
		Description: "Expose MCP server tools to the agent (Exa, etc.)",
	}, RegisterMCPSkill, globalProvider)
}
