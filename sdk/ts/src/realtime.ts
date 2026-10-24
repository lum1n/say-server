import type { components } from "./schema.gen.ts";

type Event = components["schemas"]["Event"];
type DeliveryPush = components["schemas"]["DeliveryPush"];

export type RealtimeStatus = "connecting" | "open" | "closed";

export interface RealtimeOptions {
  /** WebSocket URL; defaults to /api/v1/ws on the page's host. */
  url?: string;
  /**
   * token authenticates the socket where the session cookie can't, as in
   * a third-party iframe. It is sent as a subprotocol, since browsers
   * cannot set headers on a WebSocket.
   */
  token?: string;
  onEvent: (event: Event) => void;
  /**
   * onStream gets a streaming message as it now is, each time it grows.
   * These pushes are not replayed and may be dropped; the edit event that
   * ends the stream comes through onEvent.
   */
  onStream?: (message: Event) => void;
  /**
   * onRoomsChanged says the caller's room list may have changed: it was
   * added to a room or created one, or the socket reconnected and may have
   * missed that. Reread the list when it fires.
   */
  onRoomsChanged?: () => void;
  /**
   * onDelivery says how a message in a followed room went out to the
   * room's bridges. Like onStream it is not replayed.
   */
  onDelivery?: (push: DeliveryPush) => void;
  onStatus?: (status: RealtimeStatus) => void;
}

type Envelope = { id: string; type: string; status?: "ok" | "error" | "unknown"; payload?: unknown };

const maxBackoff = 30_000;

/**
 * Realtime follows rooms over the /ws socket. It keeps each room's cursor
 * (the highest seq it has delivered) and, when the socket drops, reconnects
 * with backoff and resubscribes from those cursors, so no event is missed
 * or delivered twice.
 */
export class Realtime {
  private ws?: WebSocket;
  private cursors = new Map<string, number>();
  private nextId = 0;
  private backoff = 500;
  private timer?: ReturnType<typeof setTimeout>;
  private closed = false;
  private opened = false;

  private readonly opts: RealtimeOptions;

  constructor(opts: RealtimeOptions) {
    this.opts = opts;
    this.connect();
  }

  /** subscribe follows roomId from after, or from where it already is. */
  subscribe(roomId: string, after = 0) {
    const cursor = Math.max(after, this.cursors.get(roomId) ?? 0);
    this.cursors.set(roomId, cursor);
    this.request("subscribe", { room_id: roomId, after: cursor });
  }

  unsubscribe(roomId: string) {
    this.cursors.delete(roomId);
    this.request("unsubscribe", { room_id: roomId });
  }

  close() {
    this.closed = true;
    clearTimeout(this.timer);
    this.ws?.close(1000);
    this.opts.onStatus?.("closed");
  }

  private connect() {
    this.opts.onStatus?.("connecting");
    const url = this.opts.url ?? `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/v1/ws`;
    const ws = this.opts.token ? new WebSocket(url, ["say.v1", `say.token.${this.opts.token}`]) : new WebSocket(url);
    this.ws = ws;
    ws.onopen = () => {
      this.backoff = 500;
      this.opts.onStatus?.("open");
      for (const [room, after] of this.cursors) this.request("subscribe", { room_id: room, after });
      if (this.opened) this.opts.onRoomsChanged?.();
      this.opened = true;
    };
    ws.onmessage = (msg) => this.receive(msg.data);
    ws.onclose = () => {
      if (this.ws !== ws || this.closed) return;
      this.opts.onStatus?.("closed");
      this.timer = setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, maxBackoff);
    };
  }

  private receive(data: unknown) {
    let env: Envelope;
    try {
      env = JSON.parse(String(data));
    } catch {
      return;
    }
    if (env.status) return;
    if (env.type === "rooms") {
      this.opts.onRoomsChanged?.();
      return;
    }
    if (env.type === "delivery") {
      const push = env.payload as DeliveryPush;
      if (this.cursors.has(push.room_id)) this.opts.onDelivery?.(push);
      return;
    }
    if (env.type === "stream") {
      const message = env.payload as Event;
      if (this.cursors.has(message.room_id)) this.opts.onStream?.(message);
      return;
    }
    if (env.type !== "event") return;
    const event = env.payload as Event;
    // A resubscribe can overlap events already delivered.
    if (event.seq <= (this.cursors.get(event.room_id) ?? Infinity)) return;
    this.cursors.set(event.room_id, event.seq);
    this.opts.onEvent(event);
  }

  private request(type: string, payload: unknown) {
    if (this.ws?.readyState !== WebSocket.OPEN) return; // sent on (re)open
    this.ws.send(JSON.stringify({ id: String(++this.nextId), type, payload }));
  }
}
