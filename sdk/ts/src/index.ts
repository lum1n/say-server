import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen.ts";

import { SayError } from "./error.ts";

export { Bot, type BotContext, type BotMessage, type BotOptions, type BotStream } from "./bot.ts";
export { SayError } from "./error.ts";
export { Realtime, type RealtimeOptions, type RealtimeStatus } from "./realtime.ts";

type Schemas = components["schemas"];
export type Health = Schemas["Health"];
export type PrincipalKind = Schemas["PrincipalKind"];
export type Principal = Schemas["Principal"];
export type SetupStatus = Schemas["SetupStatus"];
export type Setup = Schemas["Setup"];
export type Login = Schemas["Login"];
export type CreatePrincipal = Schemas["CreatePrincipal"];
export type CreatedPrincipal = Schemas["CreatedPrincipal"];
export type RoomKind = Schemas["RoomKind"];
export type Room = Schemas["Room"];
export type AgentPolicy = Schemas["AgentPolicy"];
export type UpdateRoom = Schemas["UpdateRoom"];
export type Member = Schemas["Member"];
export type Event = Schemas["Event"];
export type Part = Schemas["Part"];
export type TextPartData = Schemas["TextPartData"];
export type ToolCallPartData = Schemas["ToolCallPartData"];
export type SendMessage = Schemas["SendMessage"];
export type EditMessage = Schemas["EditMessage"];
export type StreamState = Schemas["StreamState"];
export type CreateRoom = Schemas["CreateRoom"];
export type AddMember = Schemas["AddMember"];
export type UpdateMember = Schemas["UpdateMember"];
export type LLMProvider = Schemas["LLMProvider"];
export type BuiltinAgent = Schemas["BuiltinAgent"];
export type BuiltinAgentSettings = Schemas["BuiltinAgentSettings"];
export type CreateBuiltinAgent = Schemas["CreateBuiltinAgent"];
export type MCPServer = Schemas["MCPServer"];
export type MCPServerSettings = Schemas["MCPServerSettings"];
export type ApiError = Schemas["Error"];
export type WsSubscribe = Schemas["WsSubscribe"];
export type WsUnsubscribe = Schemas["WsUnsubscribe"];
export type WsSend = Schemas["WsSend"];
export type WsError = Schemas["WsError"];
export type WsEdit = Schemas["WsEdit"];
export type WsHello = Schemas["WsHello"];
export type WsHelloReply = Schemas["WsHelloReply"];
export type WsAck = Schemas["WsAck"];
export type Trigger = Schemas["Trigger"];
export type Connector = Schemas["Connector"];
export type Widget = Schemas["Widget"];
export type WidgetSettings = Schemas["WidgetSettings"];
export type WidgetThread = Schemas["WidgetThread"];
export type EmbedConfig = Schemas["EmbedConfig"];
export type EmbedSession = Schemas["EmbedSession"];
export type EmbedSessionRequest = Schemas["EmbedSessionRequest"];
export type Network = Schemas["Network"];
export type NetworkField = Schemas["NetworkField"];
export type Bridge = Schemas["Bridge"];
export type BridgeMode = Schemas["BridgeMode"];
export type BridgeState = Schemas["BridgeState"];
export type BridgeSettings = Schemas["BridgeSettings"];
export type BridgePrompt = Schemas["BridgePrompt"];
export type BridgeLink = Schemas["BridgeLink"];
export type CreateBridgeLink = Schemas["CreateBridgeLink"];
export type RoomBridge = Schemas["RoomBridge"];
export type Delivery = Schemas["Delivery"];
export type DeliveryState = Schemas["DeliveryState"];
export type DeliveryPush = Schemas["DeliveryPush"];

/**
 * createSayClient makes a REST client. In the browser the session cookie
 * authenticates it; elsewhere pass an access token.
 */
export function createSayClient(baseUrl = "/api/v1", token?: string) {
  return createClient<paths>({ baseUrl, headers: token ? { Authorization: `Bearer ${token}` } : undefined });
}

const defaultClient = createSayClient();

export async function getHealth(client = defaultClient): Promise<Health> {
  const { data, error } = await client.GET("/health");
  if (error || !data) throw new Error("health check failed");
  return data;
}

function fail(res: Response, error: ApiError | undefined): never {
  throw new SayError(res.status, error?.error.code ?? "internal", error?.error.message ?? res.statusText);
}

export async function getSetup(client = defaultClient): Promise<SetupStatus> {
  const { data, error, response } = await client.GET("/setup");
  if (!data) fail(response, error);
  return data;
}

/** setup creates the first admin and sets the session cookie. */
export async function setup(body: Setup, client = defaultClient): Promise<Principal> {
  const { data, error, response } = await client.POST("/setup", { body });
  if (!data) fail(response, error);
  return data;
}

/** login sets the session cookie. */
export async function login(body: Login, client = defaultClient): Promise<Principal> {
  const { data, error, response } = await client.POST("/login", { body });
  if (!data) fail(response, error);
  return data;
}

export async function logout(client = defaultClient): Promise<void> {
  const { error, response } = await client.POST("/logout");
  if (!response.ok) fail(response, error);
}

export async function getMe(client = defaultClient): Promise<Principal> {
  const { data, error, response } = await client.GET("/me");
  if (!data) fail(response, error);
  return data;
}

export async function listPrincipals(client = defaultClient): Promise<Principal[]> {
  const { data, error, response } = await client.GET("/principals");
  if (!data) fail(response, error);
  return data.principals;
}

/** createPrincipal adds a person or an agent; an agent's token is only returned here. */
export async function createPrincipal(body: CreatePrincipal, client = defaultClient): Promise<CreatedPrincipal> {
  const { data, error, response } = await client.POST("/principals", { body });
  if (!data) fail(response, error);
  return data;
}

export async function listRooms(client = defaultClient): Promise<Room[]> {
  const { data, error, response } = await client.GET("/rooms");
  if (!data) fail(response, error);
  return data.rooms;
}

export async function createRoom(body: CreateRoom, client = defaultClient): Promise<Room> {
  const { data, error, response } = await client.POST("/rooms", { body });
  if (!data) fail(response, error);
  return data;
}

export async function getRoom(id: string, client = defaultClient): Promise<Room> {
  const { data, error, response } = await client.GET("/rooms/{id}", { params: { path: { id } } });
  if (!data) fail(response, error);
  return data;
}

/** updateRoom changes a room's agent settings. */
export async function updateRoom(id: string, body: UpdateRoom, client = defaultClient): Promise<Room> {
  const { data, error, response } = await client.PATCH("/rooms/{id}", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

export async function listMembers(id: string, client = defaultClient): Promise<Member[]> {
  const { data, error, response } = await client.GET("/rooms/{id}/members", { params: { path: { id } } });
  if (!data) fail(response, error);
  return data.members;
}

export async function addMember(id: string, body: AddMember, client = defaultClient): Promise<Member> {
  const { data, error, response } = await client.POST("/rooms/{id}/members", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

/** updateMember makes an agent a designated responder in a room, or stops it being one. */
export async function updateMember(
  id: string,
  principalId: string,
  body: UpdateMember,
  client = defaultClient,
): Promise<Member> {
  const { data, error, response } = await client.PATCH("/rooms/{id}/members/{principal_id}", {
    params: { path: { id, principal_id: principalId } },
    body,
  });
  if (!data) fail(response, error);
  return data;
}

export async function listBuiltinAgents(client = defaultClient): Promise<BuiltinAgent[]> {
  const { data, error, response } = await client.GET("/builtin-agents");
  if (!data) fail(response, error);
  return data.agents;
}

export async function createBuiltinAgent(body: CreateBuiltinAgent, client = defaultClient): Promise<BuiltinAgent> {
  const { data, error, response } = await client.POST("/builtin-agents", { body });
  if (!data) fail(response, error);
  return data;
}

/** updateBuiltinAgent changes the fields given; an absent api_key keeps the stored one. */
export async function updateBuiltinAgent(
  id: string,
  body: BuiltinAgentSettings,
  client = defaultClient,
): Promise<BuiltinAgent> {
  const { data, error, response } = await client.PATCH("/builtin-agents/{id}", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

/**
 * listEvents reads a page of a room's log in seq order: forward from
 * `after`, or else the page that ends before `before`, or the latest page.
 */
export async function listEvents(
  id: string,
  query: { after?: number; before?: number; limit?: number; wait?: number } = {},
  client = defaultClient,
): Promise<Event[]> {
  return (await listEventPage(id, query, client)).events;
}

/** listEventPage is listEvents with how the page's messages went out to the room's bridges. */
export async function listEventPage(
  id: string,
  query: { after?: number; before?: number; limit?: number; wait?: number } = {},
  client = defaultClient,
): Promise<{ events: Event[]; deliveries: Delivery[] }> {
  const { data, error, response } = await client.GET("/rooms/{id}/events", { params: { path: { id }, query } });
  if (!data) fail(response, error);
  return data;
}

/**
 * sendMessage posts a message. Retrying with the same idempotencyKey is
 * safe: the server returns the event the first attempt created.
 */
export async function sendMessage(
  id: string,
  body: SendMessage,
  idempotencyKey: string,
  client = defaultClient,
): Promise<Event> {
  const { data, error, response } = await client.POST("/rooms/{id}/messages", {
    params: { path: { id }, header: { "Idempotency-Key": idempotencyKey } },
    body,
  });
  if (!data) fail(response, error);
  return data;
}

/**
 * editMessage changes one of your messages. While it streams, parts
 * replace it in place; `stream: "final"` ends the stream, and a later edit
 * of a final message is logged and marks it edited. It returns the message
 * as it now is.
 */
export async function editMessage(
  id: string,
  eventId: string,
  body: EditMessage,
  client = defaultClient,
): Promise<Event> {
  const { data, error, response } = await client.PATCH("/rooms/{id}/messages/{event_id}", {
    params: { path: { id, event_id: eventId } },
    body,
  });
  if (!data) fail(response, error);
  return data;
}

/** handoffRoom hands a widget thread from its agent to the people in it. */
export async function handoffRoom(id: string, client = defaultClient): Promise<void> {
  const { error, response } = await client.POST("/rooms/{id}/handoff", { params: { path: { id } } });
  if (!response.ok) fail(response, error);
}

export async function listWidgets(client = defaultClient): Promise<Widget[]> {
  const { data, error, response } = await client.GET("/widgets");
  if (!data) fail(response, error);
  return data.widgets;
}

export async function createWidget(body: WidgetSettings, client = defaultClient): Promise<Widget> {
  const { data, error, response } = await client.POST("/widgets", { body });
  if (!data) fail(response, error);
  return data;
}

/** updateWidget changes the fields given; rotate_secret replaces the secret. */
export async function updateWidget(id: string, body: WidgetSettings, client = defaultClient): Promise<Widget> {
  const { data, error, response } = await client.PATCH("/widgets/{id}", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

/** listWidgetThreads reads my inbox: the widget threads I am in, latest activity first. */
export async function listWidgetThreads(client = defaultClient): Promise<WidgetThread[]> {
  const { data, error, response } = await client.GET("/widget-threads");
  if (!data) fail(response, error);
  return data.threads;
}

/** getEmbedConfig reads a widget's public settings by its public key. */
export async function getEmbedConfig(key: string, client = defaultClient): Promise<EmbedConfig> {
  const { data, error, response } = await client.GET("/embed/{key}", { params: { path: { key } } });
  if (!data) fail(response, error);
  return data;
}

/**
 * createEmbedSession signs a widget visitor in. Pass a client made with
 * the visitor's earlier token to get that visitor back.
 */
export async function createEmbedSession(
  key: string,
  body: EmbedSessionRequest,
  client = defaultClient,
): Promise<EmbedSession> {
  const { data, error, response } = await client.POST("/embed/{key}/session", { params: { path: { key } }, body });
  if (!data) fail(response, error);
  return data;
}

export async function listNetworks(client = defaultClient): Promise<Network[]> {
  const { data, error, response } = await client.GET("/networks");
  if (!data) fail(response, error);
  return data.networks;
}

/** listBridges returns the caller's accounts, and relay bots for admins. */
export async function listBridges(client = defaultClient): Promise<Bridge[]> {
  const { data, error, response } = await client.GET("/bridges");
  if (!data) fail(response, error);
  return data.bridges;
}

export async function createBridge(body: BridgeSettings, client = defaultClient): Promise<Bridge> {
  const { data, error, response } = await client.POST("/bridges", { body });
  if (!data) fail(response, error);
  return data;
}

/** updateBridge changes the fields given; secrets left out keep their value. */
export async function updateBridge(id: string, body: BridgeSettings, client = defaultClient): Promise<Bridge> {
  const { data, error, response } = await client.PATCH("/bridges/{id}", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

/** answerBridge answers a bridge's login prompt. */
export async function answerBridge(id: string, value: string, client = defaultClient): Promise<void> {
  const { error, response } = await client.POST("/bridges/{id}/answer", { params: { path: { id } }, body: { value } });
  if (!response.ok) fail(response, error);
}

export async function listBridgeLinks(id: string, client = defaultClient): Promise<BridgeLink[]> {
  const { data, error, response } = await client.GET("/bridges/{id}/links", { params: { path: { id } } });
  if (!data) fail(response, error);
  return data.links;
}

export async function createBridgeLink(id: string, body: CreateBridgeLink, client = defaultClient): Promise<BridgeLink> {
  const { data, error, response } = await client.POST("/bridges/{id}/links", { params: { path: { id } }, body });
  if (!data) fail(response, error);
  return data;
}

export async function deleteBridgeLink(id: string, linkId: string, client = defaultClient): Promise<void> {
  const { error, response } = await client.DELETE("/bridges/{id}/links/{link_id}", {
    params: { path: { id, link_id: linkId } },
  });
  if (!response.ok) fail(response, error);
}
