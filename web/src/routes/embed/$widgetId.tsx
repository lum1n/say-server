import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import {
  createEmbedSession,
  createSayClient,
  type EmbedConfig,
  type EmbedSession,
  type Event,
  getEmbedConfig,
  handoffRoom,
  listEvents,
  listMembers,
  type Member,
  Realtime,
  sendMessage,
} from "@say/sdk";
import { type EmbedMessage, isHostMessage, type Theme } from "@say/sdk/embed";
import { type FormEvent, type KeyboardEvent, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { applyLive, applyStream, mergeEvents } from "../../chat";

// Mounted inside the iframe that <say-chat> creates on a host page
// (docs/adr/0012-widget.md). The visitor's token lives in this frame's
// localStorage, which browsers partition per host site; cookies are not
// used, since browsers block them in third-party frames.
export const Route = createFileRoute("/embed/$widgetId")({
  component: Embed,
});

function Embed() {
  const { widgetId: key } = Route.useParams();
  const config = useQuery({ queryKey: ["embed", key], queryFn: () => getEmbedConfig(key), retry: false });
  if (config.error) return <main className="embed center muted">This chat is not available.</main>;
  if (!config.data) return <main className="embed" />;
  return <Widget widgetKey={key} config={config.data} />;
}

// parentOrigin is the origin of the page that frames us, or null when the
// page is opened on its own.
function parentOrigin(): string | null {
  if (window.parent === window) return null;
  const ancestors = (location as Location & { ancestorOrigins?: DOMStringList }).ancestorOrigins;
  if (ancestors?.length) return ancestors[0];
  try {
    return document.referrer ? new URL(document.referrer).origin : "";
  } catch {
    return "";
  }
}

type Stored = { token: string; verified: boolean };

function readStored(key: string): Stored | null {
  try {
    return JSON.parse(localStorage.getItem(`say-widget:${key}`) ?? "null");
  } catch {
    return null;
  }
}

function writeStored(key: string, s: Stored | null) {
  try {
    if (s) localStorage.setItem(`say-widget:${key}`, JSON.stringify(s));
    else localStorage.removeItem(`say-widget:${key}`);
  } catch {
    // Storage is off: the visitor lasts as long as the page.
  }
}

function applyTheme(theme: Theme) {
  for (const [name, value] of Object.entries(theme)) {
    if (name.startsWith("--say-")) document.documentElement.style.setProperty(name, value);
  }
}

function Widget({ widgetKey: key, config }: { widgetKey: string; config: EmbedConfig }) {
  const [host] = useState(parentOrigin);
  const allowed = host === null || host === location.origin || config.origins.includes(host);
  // undefined until the host's init arrives; null without an identity.
  const [identity, setIdentity] = useState<string | null | undefined>(host === null ? null : undefined);
  const [open, setOpenState] = useState(host === null);
  // Read by socket handlers, which must not wait for a render to see a
  // close: a hidden frame may render late.
  const openRef = useRef(open);
  const setOpen = (o: boolean) => {
    openRef.current = o;
    setOpenState(o);
  };
  const [closable, setClosable] = useState(false);
  const [session, setSession] = useState<EmbedSession | null>(null);
  const [error, setError] = useState<string | null>(null);

  const post = useCallback(
    (m: EmbedMessage) => {
      if (host) window.parent.postMessage(m, host);
    },
    [host],
  );

  useEffect(() => {
    if (!host || !allowed) return;
    const onMessage = (event: MessageEvent) => {
      if (event.origin !== host || event.source !== window.parent || !isHostMessage(event.data)) return;
      const m = event.data;
      switch (m.type) {
        case "init":
          applyTheme(m.theme);
          setOpen(m.open);
          setClosable(m.closable);
          setIdentity(m.identity);
          break;
        case "theme":
          applyTheme(m.theme);
          break;
        case "open":
        case "close":
          setOpen(m.type === "open");
          break;
        case "identity":
          setIdentity(m.identity);
          break;
      }
    };
    window.addEventListener("message", onMessage);
    post({ source: "say-embed", type: "ready" });
    return () => window.removeEventListener("message", onMessage);
  }, [host, allowed, post]);

  // signIn returns the visitor's session, creating it if need be.
  const signIn = useCallback(async () => {
    let stored = readStored(key);
    // A signed-out visitor never resumes the signed-in one.
    if (stored?.verified && !identity) stored = null;
    const s = await createEmbedSession(key, identity ? { identity } : {}, createSayClient("/api/v1", stored?.token));
    writeStored(key, { token: s.token, verified: !!identity });
    setSession(s);
    setError(null);
    return s;
  }, [key, identity]);

  // Resume a visitor we know, or sign in a verified one, as soon as we
  // know who it is. Anonymous newcomers sign in on their first message.
  useEffect(() => {
    if (identity === undefined) return;
    setSession(null);
    if (identity || readStored(key)) {
      signIn().catch((e: Error) => setError(e.message));
    }
  }, [identity, key, signIn]);

  if (!allowed) return <main className="embed center muted">This chat is not set up for this site.</main>;
  return (
    <main className="embed">
      <header className="embed-header">
        <h1>{config.name}</h1>
        {closable && (
          <button type="button" className="embed-close" aria-label="Close chat" onClick={() => post({ source: "say-embed", type: "close" })}>
            ×
          </button>
        )}
      </header>
      <Thread
        config={config}
        session={session}
        signIn={signIn}
        open={open}
        openRef={openRef}
        onUnread={(count) => post({ source: "say-embed", type: "unread", count })}
      />
      {error && <p className="embed-error">{error}</p>}
    </main>
  );
}

function Thread({
  config,
  session,
  signIn,
  open,
  openRef,
  onUnread,
}: {
  config: EmbedConfig;
  session: EmbedSession | null;
  signIn: () => Promise<EmbedSession>;
  open: boolean;
  openRef: { readonly current: boolean };
  onUnread: (count: number) => void;
}) {
  const [log, setLog] = useState<Event[]>([]);
  const [members, setMembers] = useState<Member[]>([]);
  const [unread, setUnread] = useState(0);
  const me = session?.principal.id;

  useEffect(() => {
    setLog([]);
    setMembers([]);
    if (!session) return;
    const client = createSayClient("/api/v1", session.token);
    const roomId = session.room.id;
    let rt: Realtime | undefined;
    let stopped = false;
    const loadMembers = () => listMembers(roomId, client).then((ms) => !stopped && setMembers(ms), () => {});
    loadMembers();
    listEvents(roomId, { limit: 100 }, client).then(
      (page) => {
        if (stopped) return;
        setLog((l) => mergeEvents(l, page));
        rt = new Realtime({
          token: session.token,
          onEvent: (e) => {
            setLog((l) => applyLive(l, e));
            if (e.type === "message" && e.sender_id !== session.principal.id && !openRef.current) setUnread((n) => n + 1);
          },
          onStream: (m) => setLog((l) => applyStream(l, m) ?? l),
          // Membership changed: someone handed the thread over.
          onRoomsChanged: loadMembers,
        });
        rt.subscribe(roomId, page.at(-1)?.seq ?? 0);
      },
      () => {},
    );
    return () => {
      stopped = true;
      rt?.close();
    };
  }, [session]);

  useEffect(() => {
    if (open) setUnread(0);
  }, [open]);
  useEffect(() => onUnread(unread), [unread]);

  const names = new Map(members.map((m) => [m.principal_id, m.display_name ?? ""]));
  const kinds = new Map(members.map((m) => [m.principal_id, m.kind]));
  const agentAnswers = members.some((m) => m.kind === "agent" && m.scopes.includes("responder"));
  const peopleHere = members.some((m) => m.kind === "human");
  const messages = log.filter((e) => e.type === "message");

  const scroller = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [log, open]);

  return (
    <>
      <div className="embed-log" ref={scroller} role="log" aria-label="Messages">
        {config.greeting && <p className="embed-msg theirs">{config.greeting}</p>}
        {messages.map((e, i) => {
          const mine = e.sender_id === me;
          const text = e.parts.flatMap((p) => (p.kind === "text" ? [(p.data as { text: string }).text] : [])).join("\n");
          if (!text && e.stream !== "streaming") return null;
          const showName = !mine && messages[i - 1]?.sender_id !== e.sender_id;
          return (
            <div key={e.seq} className={`embed-msg ${mine ? "mine" : "theirs"}`}>
              {showName && (
                <span className="embed-name">
                  {names.get(e.sender_id) || "…"}
                  {kinds.get(e.sender_id) === "agent" && <span className="embed-tag">AI</span>}
                </span>
              )}
              {text}
              {e.stream === "streaming" && <span className="caret" aria-hidden="true" />}
            </div>
          );
        })}
      </div>
      {session && agentAnswers && peopleHere && <Handoff session={session} />}
      <Composer session={session} signIn={signIn} />
    </>
  );
}

function Handoff({ session }: { session: EmbedSession }) {
  const [state, setState] = useState<"idle" | "asking" | "failed">("idle");
  return (
    <p className="embed-handoff">
      <button
        type="button"
        className="link"
        disabled={state === "asking"}
        onClick={() => {
          setState("asking");
          // The members push hides this once the agent is no longer the responder.
          handoffRoom(session.room.id, createSayClient("/api/v1", session.token)).catch(() => setState("failed"));
        }}
      >
        Talk to a person
      </button>
      {state === "failed" && " (failed, try again)"}
    </p>
  );
}

function Composer({ session, signIn }: { session: EmbedSession | null; signIn: () => Promise<EmbedSession> }) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  // One key per message, kept across retries, so a retry never sends twice.
  const keyRef = useRef<string>(crypto.randomUUID());

  const send = async (e?: FormEvent) => {
    e?.preventDefault();
    const body = text.trim();
    if (!body || sending) return;
    setSending(true);
    setFailed(null);
    try {
      const s = session ?? (await signIn());
      await sendMessage(
        s.room.id,
        { parts: [{ kind: "text", data: { text: body } }] },
        keyRef.current,
        createSayClient("/api/v1", s.token),
      );
      setText("");
      keyRef.current = crypto.randomUUID();
    } catch (err) {
      setFailed((err as Error).message);
    } finally {
      setSending(false);
    }
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      send();
    }
  };
  return (
    <form className="embed-composer" onSubmit={send}>
      <textarea
        aria-label="Message"
        placeholder="Write a message…"
        rows={1}
        value={text}
        maxLength={10000}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKey}
      />
      <button type="submit" disabled={sending || !text.trim()}>
        Send
      </button>
      {failed && <p className="embed-error">Not sent: {failed}</p>}
    </form>
  );
}
