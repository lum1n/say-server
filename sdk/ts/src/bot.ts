import { SayError } from "./error.ts";
import type { components } from "./schema.gen.ts";

type Schemas = components["schemas"];
type Event = Schemas["Event"];
type Part = Schemas["Part"];
type Principal = Schemas["Principal"];
type Trigger = Schemas["Trigger"];

export interface BotOptions {
  /** The server, e.g. "http://localhost:7290". */
  url: string;
  /** The agent's access token. */
  token: string;
  /** Shown to people while the bot is online. */
  name?: string;
  version?: string;
  capabilities?: string[];
  /** Gets handler failures and connection errors; defaults to console.error. */
  onError?: (err: unknown) => void;
}

/** A message that asks the bot for an answer. */
export interface BotMessage {
  /** The message's id; replies set it as caused_by. */
  id: string;
  roomId: string;
  senderId: string;
  /** The text parts, one per line. */
  text: string;
  parts: Part[];
  trigger: Trigger;
  /** The event that brought it: the message, or the edit that ended its stream. */
  event: Event;
}

export interface BotStream {
  /** append adds text to the reply; changes are sent at most every 250 ms. */
  append(text: string): void;
  /** end makes the reply final, with text when given, else with what was appended. */
  end(text?: string): Promise<void>;
}

export interface BotContext {
  /**
   * reply answers the message. Its idempotency key is derived from the
   * message, so a message delivered again is answered once.
   */
  reply(text: string): Promise<Event>;
  /** stream starts a reply that grows as the bot writes it. */
  stream(): Promise<BotStream>;
  /** send posts to the message's room without replying to it. */
  send(text: string): Promise<Event>;
}

type Envelope = {
  id: string;
  type: string;
  status?: "ok" | "error" | "unknown";
  payload?: unknown;
  trigger?: Trigger;
};

type Pending = { resolve: (v: unknown) => void; reject: (e: unknown) => void };

const maxBackoff = 30_000;
const streamEvery = 250;
const ackEvery = 200;

/**
 * Bot is an agent connected over the /ws bot session: the server follows
 * all of the agent's rooms, says which messages ask it for an answer, and
 * keeps its cursors, so a restarted bot carries on where it left off.
 * Events in one room are handled one at a time, in order; each is acked
 * once its handler finishes. Needs a WebSocket that takes headers (Node 22
 * or later).
 */
export class Bot {
  /** The agent, once connected. */
  me?: Principal;

  private ws?: WebSocket;
  private nextId = 0;
  private pending = new Map<string, Pending>();
  private messageHandlers: ((msg: BotMessage, ctx: BotContext) => unknown)[] = [];
  private eventHandlers: ((event: Event) => unknown)[] = [];
  private queues = new Map<string, Promise<void>>();
  private acks = new Map<string, number>();
  private ackTimer?: ReturnType<typeof setTimeout>;
  private backoff = 500;
  private timer?: ReturnType<typeof setTimeout>;
  private stopped = false;
  private started?: { resolve: () => void; reject: (e: unknown) => void };

  private readonly opts: BotOptions;

  constructor(opts: BotOptions) {
    this.opts = opts;
  }

  /** on("message") gets the messages that ask the bot for an answer. */
  on(type: "message", handler: (msg: BotMessage, ctx: BotContext) => unknown): this;
  /** on("event") gets every event in the bot's rooms, its own included. */
  on(type: "event", handler: (event: Event) => unknown): this;
  on(type: "message" | "event", handler: (...args: never[]) => unknown): this {
    if (type === "message") this.messageHandlers.push(handler as never);
    else this.eventHandlers.push(handler as never);
    return this;
  }

  /** start connects and resolves once the first session is open. */
  start(): Promise<void> {
    this.stopped = false;
    return new Promise((resolve, reject) => {
      this.started = { resolve, reject };
      this.connect();
    });
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.timer);
    this.flushAcks();
    this.ws?.close(1000);
  }

  /**
   * send posts parts to a room. causedBy is the message that prompted it;
   * replyTo, a message in the room, also shows it as a quote.
   */
  send(
    roomId: string,
    parts: Part[],
    opts: { causedBy?: string; replyTo?: string; idempotencyKey?: string; stream?: "streaming" | "final" } = {},
  ): Promise<Event> {
    return this.request("send", {
      room_id: roomId,
      parts,
      caused_by: opts.causedBy,
      reply_to: opts.replyTo,
      idempotency_key: opts.idempotencyKey,
      stream: opts.stream,
    }) as Promise<Event>;
  }

  /** react adds the bot's reaction to a message: one emoji, or a :shortcode:. */
  react(roomId: string, eventId: string, key: string): Promise<Event> {
    return this.request("react", { room_id: roomId, event_id: eventId, key }) as Promise<Event>;
  }

  /** unreact takes the bot's reaction back. */
  unreact(roomId: string, eventId: string, key: string): Promise<Event> {
    return this.request("unreact", { room_id: roomId, event_id: eventId, key }) as Promise<Event>;
  }

  /** edit changes one of the bot's messages, or streams it. */
  edit(roomId: string, eventId: string, edit: { parts?: Part[]; stream?: "streaming" | "final" }): Promise<Event> {
    return this.request("edit", { room_id: roomId, event_id: eventId, ...edit }) as Promise<Event>;
  }

  private connect() {
    const base = this.opts.url.replace(/^http/, "ws").replace(/\/$/, "");
    // Node's WebSocket takes headers as a second argument; browsers' don't.
    const WS = WebSocket as unknown as new (url: string, init: unknown) => WebSocket;
    const ws = new WS(`${base}/api/v1/ws`, { headers: { Authorization: `Bearer ${this.opts.token}` } });
    this.ws = ws;
    ws.onopen = () => {
      this.request("hello", {
        name: this.opts.name ?? "",
        version: this.opts.version ?? "",
        capabilities: this.opts.capabilities ?? [],
      }).then(
        (reply) => {
          this.backoff = 500;
          this.me = (reply as { principal: Principal }).principal;
          this.started?.resolve();
          this.started = undefined;
        },
        (err) => {
          // A refused hello will be refused again: give up.
          this.started?.reject(err);
          this.started = undefined;
          this.report(err);
          this.stop();
        },
      );
    };
    ws.onmessage = (msg) => this.receive(String(msg.data));
    ws.onerror = () => {};
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      for (const p of this.pending.values()) p.reject(new SayError(0, "disconnected", "the connection closed; the outcome is unknown"));
      this.pending.clear();
      if (this.stopped) return;
      this.report(new Error(`connection closed (${ev.code}); reconnecting in ${this.backoff} ms`));
      this.timer = setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, maxBackoff);
    };
  }

  private receive(data: string) {
    let env: Envelope;
    try {
      env = JSON.parse(data);
    } catch {
      return;
    }
    if (env.status) {
      const p = this.pending.get(env.id);
      if (!p) return;
      this.pending.delete(env.id);
      if (env.status === "ok") p.resolve(env.payload);
      else {
        const e = env.payload as { code?: string; message?: string; status?: number };
        p.reject(new SayError(e?.status ?? 0, e?.code ?? "internal", e?.message ?? "request failed"));
      }
      return;
    }
    if (env.type !== "event") return;
    const event = env.payload as Event;
    const room = event.room_id;
    const ws = this.ws;
    const prev = this.queues.get(room) ?? Promise.resolve();
    const next = prev.then(async () => {
      await this.handle(event, env.trigger);
      // Events handled on a socket that has since closed come again.
      if (this.ws === ws) this.ack(room, event.seq);
    });
    this.queues.set(room, next);
  }

  private async handle(event: Event, trigger?: Trigger) {
    for (const h of this.eventHandlers) {
      try {
        await h(event);
      } catch (err) {
        this.report(err);
      }
    }
    if (!trigger || this.messageHandlers.length === 0) return;
    const parts = event.parts;
    const msg: BotMessage = {
      id: event.type === "edit" && event.edits ? event.edits : event.id,
      roomId: event.room_id,
      senderId: event.sender_id,
      text: textOf(parts),
      parts,
      trigger,
      event,
    };
    const ctx = this.context(msg);
    for (const h of this.messageHandlers) {
      try {
        await h(msg, ctx);
      } catch (err) {
        this.report(err);
      }
    }
  }

  private context(msg: BotMessage): BotContext {
    const key = `bot:${msg.id}`;
    return {
      reply: (text) => this.send(msg.roomId, [textPart(text)], { causedBy: msg.id, idempotencyKey: key }),
      send: (text) => this.send(msg.roomId, [textPart(text)]),
      stream: async () => {
        const started = await this.send(msg.roomId, [], { causedBy: msg.id, idempotencyKey: key, stream: "streaming" });
        if (started.stream === "final") return doneStream; // answered before
        return this.streamer(msg.roomId, started.id);
      },
    };
  }

  private streamer(roomId: string, eventId: string): BotStream {
    let text = "";
    let sent = "";
    let timer: ReturnType<typeof setTimeout> | undefined;
    let writing = Promise.resolve();
    const flush = () => {
      timer = undefined;
      if (text === sent || text === "") return;
      const now = text;
      writing = writing
        .then(() => this.edit(roomId, eventId, { parts: [textPart(now)], stream: "streaming" }))
        .then(
          () => {
            sent = now;
          },
          (err) => this.report(err),
        );
    };
    return {
      append: (more) => {
        text += more;
        timer ??= setTimeout(flush, streamEvery);
      },
      end: async (final) => {
        clearTimeout(timer);
        await writing;
        const body = final ?? text;
        await this.edit(roomId, eventId, { parts: body ? [textPart(body)] : undefined, stream: "final" });
      },
    };
  }

  private ack(roomId: string, seq: number) {
    this.acks.set(roomId, Math.max(seq, this.acks.get(roomId) ?? 0));
    this.ackTimer ??= setTimeout(() => this.flushAcks(), ackEvery);
  }

  private flushAcks() {
    clearTimeout(this.ackTimer);
    this.ackTimer = undefined;
    for (const [room, seq] of this.acks) {
      this.request("ack", { room_id: room, seq }).catch((err) => this.report(err));
    }
    this.acks.clear();
  }

  private request(type: string, payload: unknown): Promise<unknown> {
    const ws = this.ws;
    if (ws?.readyState !== WebSocket.OPEN) {
      return Promise.reject(new SayError(0, "disconnected", "not connected"));
    }
    const id = String(++this.nextId);
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, type, payload }));
    });
  }

  private report(err: unknown) {
    (this.opts.onError ?? console.error)(err);
  }
}

const doneStream: BotStream = { append() {}, end: async () => {} };

function textPart(text: string): Part {
  return { kind: "text", data: { text } };
}

function textOf(parts: Part[]): string {
  return parts
    .filter((p) => p.kind === "text")
    .map((p) => String((p.data as { text?: unknown }).text ?? ""))
    .join("\n");
}
