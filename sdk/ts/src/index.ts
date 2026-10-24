import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen";

export { Realtime, type RealtimeOptions, type RealtimeStatus } from "./realtime";

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
export type SendMessage = Schemas["SendMessage"];
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

export function createSayClient(baseUrl = "/api/v1") {
  return createClient<paths>({ baseUrl });
}

const defaultClient = createSayClient();

export async function getHealth(client = defaultClient): Promise<Health> {
  const { data, error } = await client.GET("/health");
  if (error || !data) throw new Error("health check failed");
  return data;
}

/** SayError carries the API's error code, e.g. "invalid_credentials". */
export class SayError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "SayError";
  }
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
  const { data, error, response } = await client.GET("/rooms/{id}/events", { params: { path: { id }, query } });
  if (!data) fail(response, error);
  return data.events;
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
