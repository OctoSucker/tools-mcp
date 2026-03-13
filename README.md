# tools-mcp

将 MCP (Model Context Protocol) 服务器的工具暴露给 OctoSucker Agent，使 LLM 可调用任意 MCP 服务（如 Exa、Context7、OpenBB 等）提供的工具。

## 工具

- **按 server 动态注册**：每个配置的 server 在加载时会连接 MCP、拉取 `ListTools`，将远端每个工具注册为本地 Tool，名称为 `mcp_{id}_{工具名}`（例如 `mcp_exa_search`、`mcp_context7_*`）。
- **元信息工具（每个 server）**：`mcp_{id}_meta` — 返回该 server 的 id、url、transport、description、setup、工具数量；server 离线时也可调用。
- **聚合工具**：`mcp_list_servers` — 返回所有已配置的 MCP server 及其工具列表（id、url、transport、description、setup、tools），供 LLM 了解当前可用的 MCP 能力。

未配置 `servers` 或列表为空时，不注册任何 MCP 工具。

## 配置

在 Agent 配置的 `tool_providers["github.com/OctoSucker/tools-mcp"]` 下指定 `servers` 列表，每项包含：

| 键 | 说明 |
|------|------|
| `id` | 服务器标识（必填），用于生成工具名前缀，如 `mcp_exa_search`。 |
| `url` | MCP 端点 URL（必填，stdio 已不支持）。 |
| `transport` | 可选：`streamable`（默认）或 `sse`。若连 OpenBB/FastMCP 出现 400，可改为 `sse`。 |
| `description` | 可选，服务器描述。 |
| `setup` | 可选，字符串数组，说明如何启动/配置该 MCP 服务。 |

示例（`config/agent_config.json`）：

```json
"github.com/OctoSucker/tools-mcp": {
  "servers": [
    { "id": "exa", "url": "https://mcp.exa.ai/mcp" },
    { "id": "context7", "url": "https://mcp.context7.com/mcp" },
    { "id": "openbb", "url": "http://127.0.0.1:8001/mcp", "transport": "sse" }
  ]
}
```

## 行为

- **Init**：仅解析 `servers`，不建立连接。
- **Register**：对每个 server 连接 MCP、拉取 `ListTools`，将每个远端工具注册为 `mcp_{id}_{工具名}`，并注册 `mcp_{id}_meta`；最后注册 `mcp_list_servers`。`transport` 为 `stdio` 的 server 会被跳过（已不支持）。
- **调用**：Agent 调用 `mcp_*` 时按需连接对应 MCP、执行 `CallTool`，将结果返回。单次调用采用「按需连接」，结束后关闭，避免长连接问题。

## 依赖

- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)（官方 Go MCP SDK，Streamable HTTP / SSE 客户端）

## 示例 MCP 服务

- **Exa 搜索**：`https://mcp.exa.ai/mcp`
- **Context7**：`https://mcp.context7.com/mcp`
- **OpenBB**：需本地启动 `openbb-mcp --transport sse --port 8001`，配置 `"url": "http://127.0.0.1:8001/mcp", "transport": "sse"`

其他支持 Streamable HTTP 或 SSE 的 MCP 服务均可按上述格式加入 `servers`。

## 安装

主项目中：

```bash
go get github.com/OctoSucker/tools-mcp@latest
```

并保留空白导入：`_ "github.com/OctoSucker/tools-mcp"`。
