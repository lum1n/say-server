// postMessage API between the <say-chat> loader (host page) and the /embed
// iframe on /embed/$widgetId.

export type HostMessage =
  | { source: "say-host"; type: "open" }
  | { source: "say-host"; type: "close" }
  | { source: "say-host"; type: "identity"; token: string }
  | { source: "say-host"; type: "theme"; theme: Record<string, string> };

export type EmbedMessage =
  | { source: "say-embed"; type: "ready" }
  | { source: "say-embed"; type: "unread"; count: number };

export function isHostMessage(data: unknown): data is HostMessage {
  return (
    typeof data === "object" &&
    data !== null &&
    (data as { source?: unknown }).source === "say-host"
  );
}

export function isEmbedMessage(data: unknown): data is EmbedMessage {
  return (
    typeof data === "object" &&
    data !== null &&
    (data as { source?: unknown }).source === "say-embed"
  );
}
