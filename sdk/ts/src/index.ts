import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen";

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
