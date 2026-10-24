import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen";

export type Health = components["schemas"]["Health"];
export type PrincipalKind = components["schemas"]["PrincipalKind"];
export type RoomKind = components["schemas"]["RoomKind"];
export type EventType = components["schemas"]["EventType"];
export type AgentSettingsChange = components["schemas"]["AgentSettingsChange"];
export type Handoff = components["schemas"]["Handoff"];

export function createSayClient(baseUrl = "/api/v1") {
  return createClient<paths>({ baseUrl });
}

const defaultClient = createSayClient();

export async function getHealth(client = defaultClient): Promise<Health> {
  const { data, error } = await client.GET("/health");
  if (error || !data) throw new Error("health check failed");
  return data;
}
