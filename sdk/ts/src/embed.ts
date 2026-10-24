// postMessage API between the <say-chat> loader (host page) and the /embed
// iframe on /embed/$widgetId (docs/adr/0012-widget.md).
//
// The embed page says ready; the loader answers with init and then sends
// the other host messages as its attributes change. Theme keys are CSS
// custom properties named --say-*.

export type Theme = Record<string, string>;

export type HostMessage =
  | {
      source: "say-host";
      type: "init";
      /** identity is a signed identity token from the host site, if any. */
      identity: string | null;
      theme: Theme;
      open: boolean;
      /** closable says the panel can be closed, so the embed shows a close button. */
      closable: boolean;
    }
  | { source: "say-host"; type: "open" }
  | { source: "say-host"; type: "close" }
  | { source: "say-host"; type: "identity"; identity: string | null }
  | { source: "say-host"; type: "theme"; theme: Theme };

export type EmbedMessage =
  | { source: "say-embed"; type: "ready" }
  | { source: "say-embed"; type: "unread"; count: number }
  /** close asks the host to close the panel (its close button). */
  | { source: "say-embed"; type: "close" };

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
