-- Cost budgets for built-in agents (docs/adr/0009-built-in-agents.md).
-- Money is in micro-dollars (1e-6 USD). Prices are per million tokens.
ALTER TABLE builtin_agents ADD COLUMN input_price BIGINT NOT NULL DEFAULT 0;
ALTER TABLE builtin_agents ADD COLUMN output_price BIGINT NOT NULL DEFAULT 0;
-- 0 is unlimited.
ALTER TABLE builtin_agents ADD COLUMN daily_cost_budget BIGINT NOT NULL DEFAULT 0;
-- What the day's calls cost at the prices set when they were made.
ALTER TABLE agent_usage ADD COLUMN cost BIGINT NOT NULL DEFAULT 0;
