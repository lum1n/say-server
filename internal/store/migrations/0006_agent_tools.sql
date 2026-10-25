-- The MCP servers whose tools a built-in agent may call, as a JSON array of
-- {name, url, authorization} (docs/adr/0009-built-in-agents.md).
ALTER TABLE builtin_agents ADD COLUMN mcp_servers TEXT NOT NULL DEFAULT '[]';
