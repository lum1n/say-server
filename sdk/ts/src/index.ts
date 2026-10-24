import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen";

type Schemas = components["schemas"];
export type Health = Schemas["Health"];
export type PrincipalKind = Schemas["PrincipalKind"];
export type Principal = Schemas["Principal"];
export type RoomKind = Schemas["RoomKind"];
export type Room = Schemas["Room"];
export type Member = Schemas["Member"];
export type Event = Schemas["Event"];
export type Part = Schemas["Part"];
export type SendMessage = Schemas["SendMessage"];
export type ApiError = Schemas["Error"];
export type WsSubscribe = Schemas["WsSubscribe"];
export type WsUnsubscribe = Schemas["WsUnsubscribe"];
export type WsSend = Schemas["WsSend"];
export type WsError = Schemas["WsError"];

export function createSayClient(baseUrl = "/api/v1") {
  return createClient<paths>({ baseUrl });
}

const defaultClient = createSayClient();

export async function getHealth(client = defaultClient): Promise<Health> {
  const { data, error } = await client.GET("/health");
  if (error || !data) throw new Error("health check failed");
  return data;
}
