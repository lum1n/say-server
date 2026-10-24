import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  addMember,
  type AgentPolicy,
  type Event,
  getMe,
  getRoom,
  listEvents,
  listMembers,
  type Member,
  type Principal,
  type Room,
  SayError,
  sendMessage,
  type ToolCallPartData,
  updateMember,
  updateRoom,
} from "@say/sdk";
import { type FormEvent, type KeyboardEvent, useEffect, useLayoutEffect, useRef, useState } from "react";
import { keys, mergeEvents, useDirectory, useRealtime } from "../../chat";
import { FormError } from "../../components/form";

export const Route = createFileRoute("/_app/rooms/$roomId")({
  component: RoomPage,
});

const pageSize = 50;

function RoomPage() {
  const { roomId } = Route.useParams();
  const room = useQuery({ queryKey: keys.room(roomId), queryFn: () => getRoom(roomId), retry: false });
  if (room.error) return <main className="empty muted">{room.error.message}</main>;
  return (
    <main className="room">
      <header className="room-header">
        <Link to="/" className="back">‹ Rooms</Link>
        <h1>{room.data ? room.data.name || "Untitled room" : "…"}</h1>
        {room.data && <PauseAgents room={room.data} />}
      </header>
      {/* key: a fresh log, composer and member list per room */}
      <Log key={`log-${roomId}`} roomId={roomId} />
      <Members key={`members-${roomId}`} roomId={roomId} room={room.data} />
    </main>
  );
}

// Log shows the room's events and the composer. The latest page is read
// over HTTP; the socket then follows the room from its last seq and keeps
// the cache current, so the query itself never refetches.
function Log({ roomId }: { roomId: string }) {
  const queryClient = useQueryClient();
  const rt = useRealtime();
  const dir = useDirectory();
  const events = useQuery({
    queryKey: keys.events(roomId),
    queryFn: async () => mergeEvents(queryClient.getQueryData(keys.events(roomId)), await listEvents(roomId, { limit: pageSize })),
    staleTime: Infinity,
  });
  const loaded = events.isSuccess;
  useEffect(() => {
    if (!rt || !loaded) return;
    const log = queryClient.getQueryData<Event[]>(keys.events(roomId)) ?? [];
    rt.subscribe(roomId, log.at(-1)?.seq ?? 0);
    return () => rt.unsubscribe(roomId);
  }, [rt, loaded, roomId, queryClient]);

  // Someone new spoke: their principal was created after the directory loaded.
  const unknown = events.data?.some((e) => !dir.byId.has(e.sender_id));
  useEffect(() => {
    if (unknown && !dir.isFetching) dir.refetch();
  }, [unknown]);

  const earlier = useMutation({
    mutationFn: (before: number) => listEvents(roomId, { before, limit: pageSize }),
    onSuccess: (page) => queryClient.setQueryData<Event[]>(keys.events(roomId), (log) => mergeEvents(log, page)),
  });

  const list = events.data ?? [];
  const first = list[0]?.seq;
  // Edits are in the log for its cursor; the messages show their result.
  const messages = list.filter((e) => e.type === "message");
  const scroller = useRef<HTMLDivElement>(null);
  const atBottom = useRef(true);
  // Also when a streaming message grows.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el && atBottom.current) el.scrollTop = el.scrollHeight;
  }, [list]);

  return (
    <>
      <div
        className="log"
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget;
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        role="log"
        aria-label="Messages"
      >
        {first !== undefined && first > 1 && (
          <button
            type="button"
            className="link earlier"
            onClick={() => {
              atBottom.current = false;
              earlier.mutate(first);
            }}
            disabled={earlier.isPending}
          >
            Load earlier messages
          </button>
        )}
        {events.isError && <p className="muted">{events.error.message}</p>}
        {loaded && messages.length === 0 && <p className="muted">No messages yet. Say something.</p>}
        {messages.map((e, i) => (
          <Message key={e.seq} event={e} sender={dir.byId.get(e.sender_id)} grouped={sameSpeaker(messages[i - 1], e)} />
        ))}
      </div>
      <Composer roomId={roomId} onSent={() => (atBottom.current = true)} />
    </>
  );
}

// sameSpeaker reports whether e continues prev's run of messages.
function sameSpeaker(prev: Event | undefined, e: Event) {
  return (
    prev?.type === "message" &&
    prev.sender_id === e.sender_id &&
    Date.parse(e.created_at) - Date.parse(prev.created_at) < 5 * 60_000
  );
}

const time = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });

function Message({ event, sender, grouped }: { event: Event; sender?: Principal; grouped: boolean }) {
  const streaming = event.stream === "streaming";
  return (
    <article className={["msg", grouped && "grouped", streaming && "streaming"].filter(Boolean).join(" ")} aria-busy={streaming}>
      {!grouped && (
        <header>
          <strong>{sender?.display_name ?? "…"}</strong>
          {sender?.kind === "agent" && <span className="badge">agent</span>}
          <time dateTime={event.created_at}>{time.format(new Date(event.created_at))}</time>
        </header>
      )}
      {streaming && event.parts.length === 0 && <p className="text muted">…</p>}
      {event.parts.map((p, i) =>
        p.kind === "text" ? (
          <p key={i} className="text">
            {String(p.data.text)}
          </p>
        ) : p.kind === "tool-call" ? (
          <ToolCall key={i} call={p.data as ToolCallPartData} />
        ) : (
          <p key={i} className="muted">
            [{p.kind}]
          </p>
        ),
      )}
      {event.edited_at && (
        <span className="edited muted" title={`Edited ${time.format(new Date(event.edited_at))}`}>
          edited
        </span>
      )}
    </article>
  );
}

// ToolCall shows a tool the sender used, with its arguments and error a
// click away.
function ToolCall({ call }: { call: ToolCallPartData }) {
  const label =
    call.status === "running" ? `Using ${call.name}…` : call.status === "failed" ? `${call.name} failed` : `Used ${call.name}`;
  const input = call.input && Object.keys(call.input).length > 0 ? JSON.stringify(call.input, null, 2) : "";
  if (!input && !call.error) {
    return <div className={`tool ${call.status}`}>{label}</div>;
  }
  return (
    <details className={`tool ${call.status}`}>
      <summary>{label}</summary>
      {call.error && <div className="tool-error">{call.error}</div>}
      {input && <pre>{input}</pre>}
    </details>
  );
}

// A send's state. "unknown" means the request may or may not have reached
// the server; it is never retried by itself, but retrying by hand reuses
// the idempotency key, so it cannot post twice.
type Pending = { key: string; text: string; state: "sending" | "unknown" | "failed"; error?: string };

// newKey makes an idempotency key. crypto.randomUUID needs a secure
// context, and a LAN instance may be served over plain HTTP.
function newKey() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
}

function Composer({ roomId, onSent }: { roomId: string; onSent: () => void }) {
  const queryClient = useQueryClient();
  const [text, setText] = useState("");
  const [pending, setPending] = useState<Pending[]>([]);
  const update = (key: string, p: Partial<Pending> | null) =>
    setPending((ps) => (p ? ps.map((x) => (x.key === key ? { ...x, ...p } : x)) : ps.filter((x) => x.key !== key)));

  const send = async ({ key, text }: Pending) => {
    update(key, { state: "sending", error: undefined });
    try {
      const e = await sendMessage(roomId, { parts: [{ kind: "text", data: { text } }] }, key);
      queryClient.setQueryData<Event[]>(keys.events(roomId), (log) => mergeEvents(log, [e]));
      update(key, null);
    } catch (err) {
      // A 4xx was a definite refusal; anything else leaves the outcome open.
      const refused = err instanceof SayError && err.status >= 400 && err.status < 500;
      update(key, { state: refused ? "failed" : "unknown", error: err instanceof Error ? err.message : String(err) });
    }
  };

  const submit = (e?: FormEvent) => {
    e?.preventDefault();
    const body = text.trim();
    if (!body) return;
    const p: Pending = { key: newKey(), text: body, state: "sending" };
    setPending((ps) => [...ps, p]);
    setText("");
    onSent();
    send(p);
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="composer">
      {pending.map((p) => (
        <div key={p.key} className={`pending ${p.state}`}>
          <span className="text">{p.text}</span>
          {p.state === "sending" && <span className="muted">Sending…</span>}
          {p.state !== "sending" && (
            <span role="alert">
              {p.state === "unknown" ? "Not confirmed" : "Not sent"}: {p.error}{" "}
              {p.state === "unknown" && (
                <button type="button" className="link" onClick={() => send(p)}>
                  Retry
                </button>
              )}{" "}
              <button type="button" className="link" onClick={() => update(p.key, null)}>
                Dismiss
              </button>
            </span>
          )}
        </div>
      ))}
      <form onSubmit={submit}>
        <textarea
          aria-label="Message"
          placeholder="Message"
          rows={1}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKey}
        />
        <button type="submit" disabled={!text.trim()}>
          Send
        </button>
      </form>
    </div>
  );
}

// useCanEditAgents reports whether I may change the room's agent settings:
// people who own or run the room, and instance admins in it.
function useCanEditAgents(roomId: string) {
  const me = useQuery({ queryKey: keys.me, queryFn: () => getMe() });
  const members = useQuery({ queryKey: keys.members(roomId), queryFn: () => listMembers(roomId) });
  const mine = members.data?.find((m) => m.principal_id === me.data?.id);
  return (
    me.data?.kind === "human" && !!mine && (mine.role === "owner" || mine.role === "admin" || !!me.data.admin)
  );
}

function useUpdateAgents(roomId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (agents: Partial<AgentPolicy>) => updateRoom(roomId, { agents }),
    onSuccess: (room) => {
      queryClient.setQueryData(keys.room(roomId), room);
      queryClient.invalidateQueries({ queryKey: keys.rooms });
    },
  });
}

// PauseAgents shows that agents are paused, and lets those who may switch it.
function PauseAgents({ room }: { room: Room }) {
  const canEdit = useCanEditAgents(room.id);
  const update = useUpdateAgents(room.id);
  const paused = room.agents.paused;
  return (
    <span className="agents-switch">
      {paused && <span className="badge">agents paused</span>}
      {canEdit && (
        <button
          type="button"
          className="link"
          disabled={update.isPending}
          onClick={() => update.mutate({ paused: !paused })}
          title={paused ? "Let agents send again" : "Stop every agent from sending in this room"}
        >
          {paused ? "Resume agents" : "Pause agents"}
        </button>
      )}
      {update.error && <span role="alert"> {update.error.message}</span>}
    </span>
  );
}

// AgentLimits edits how far agents may go in the room.
function AgentLimits({ room }: { room: Room }) {
  const update = useUpdateAgents(room.id);
  const a = room.agents;
  const [hops, setHops] = useState(String(a.max_hop_count));
  const [turns, setTurns] = useState(String(a.turn_limit));
  const [minutes, setMinutes] = useState(String(Math.round(a.turn_window_seconds / 60)));
  const [rate, setRate] = useState(String(a.rate_limit_per_minute));
  const submit = (e: FormEvent) => {
    e.preventDefault();
    update.mutate({
      max_hop_count: Number(hops),
      turn_limit: Number(turns),
      turn_window_seconds: Number(minutes) * 60,
      rate_limit_per_minute: Number(rate),
    });
  };
  return (
    <form className="agent-limits" onSubmit={submit}>
      <h2>Agent limits</h2>
      <label>
        Replies in a row
        <input type="number" min={1} max={64} required value={hops} onChange={(e) => setHops(e.target.value)} />
      </label>
      <label>
        Messages from all agents
        <span className="per">
          <input type="number" min={1} max={10000} required value={turns} onChange={(e) => setTurns(e.target.value)} />
          per
          <input type="number" min={1} max={1440} required value={minutes} onChange={(e) => setMinutes(e.target.value)} />
          min
        </span>
      </label>
      <label>
        Messages per agent per minute
        <input type="number" min={1} max={600} required value={rate} onChange={(e) => setRate(e.target.value)} />
      </label>
      <button type="submit" disabled={update.isPending}>
        Save limits
      </button>
      {update.isSuccess && !update.isPending && <span className="muted">Saved.</span>}
      <FormError error={update.error} />
    </form>
  );
}

function Members({ roomId, room }: { roomId: string; room?: Room }) {
  const queryClient = useQueryClient();
  const dir = useDirectory();
  const me = useQuery({ queryKey: keys.me, queryFn: () => getMe() });
  const members = useQuery({ queryKey: keys.members(roomId), queryFn: () => listMembers(roomId) });
  const [pick, setPick] = useState("");
  const add = useMutation({
    mutationFn: () => addMember(roomId, { principal_id: pick, role: "member" }),
    onSuccess: () => {
      setPick("");
      queryClient.invalidateQueries({ queryKey: keys.members(roomId) });
    },
  });
  // While a responder change is on its way, the box shows where it is going.
  const [changing, setChanging] = useState<Record<string, boolean>>({});
  const respond = useMutation({
    mutationFn: ({ id, responder }: { id: string; responder: boolean }) => updateMember(roomId, id, { responder }),
    onMutate: ({ id, responder }) => setChanging((c) => ({ ...c, [id]: responder })),
    onSuccess: (m) =>
      queryClient.setQueryData<Member[]>(keys.members(roomId), (list) =>
        list?.map((x) => (x.principal_id === m.principal_id ? m : x)),
      ),
    onSettled: (_m, _err, { id }) =>
      setChanging((c) => {
        const { [id]: _, ...rest } = c;
        return rest;
      }),
  });

  const list = members.data ?? [];
  const mine = list.find((m) => m.principal_id === me.data?.id);
  const canAdd = mine?.role === "owner" || mine?.role === "admin";
  const canEditAgents = useCanEditAgents(roomId);
  const inRoom = new Set(list.map((m) => m.principal_id));
  const candidates = dir.list.filter((p) => !inRoom.has(p.id) && (p.kind === "human" || p.kind === "agent"));
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };

  return (
    <aside className="members" aria-label="Members">
      <h2>Members</h2>
      <ul>
        {list.map((m) => {
          const p = dir.byId.get(m.principal_id);
          const responder = changing[m.principal_id] ?? m.scopes.includes("responder");
          return (
            <li key={m.principal_id}>
              {p?.display_name ?? "…"}
              {p?.kind === "agent" && <span className="badge">agent</span>}
              {m.role !== "member" && <span className="muted"> {m.role}</span>}
              {p?.kind === "agent" &&
                (canEditAgents ? (
                  <label className="responder" title="A built-in agent answers every message here, not only @mentions">
                    <input
                      type="checkbox"
                      checked={responder}
                      disabled={m.principal_id in changing}
                      onChange={(e) => {
                        const responder = e.target.checked;
                        setChanging((c) => ({ ...c, [m.principal_id]: responder }));
                        respond.mutate({ id: m.principal_id, responder });
                      }}
                    />
                    answers everything
                  </label>
                ) : (
                  responder && <span className="muted"> answers everything</span>
                ))}
            </li>
          );
        })}
      </ul>
      <FormError error={respond.error} />
      {canAdd && candidates.length > 0 && (
        <form onSubmit={submit}>
          <select aria-label="Add a member" value={pick} onChange={(e) => setPick(e.target.value)} required>
            <option value="">Add someone…</option>
            {candidates.map((p) => (
              <option key={p.id} value={p.id}>
                {p.display_name}
                {p.kind === "agent" ? " (agent)" : ""}
              </option>
            ))}
          </select>
          <button type="submit" disabled={!pick || add.isPending}>
            Add
          </button>
          <FormError error={add.error} />
        </form>
      )}
      {canEditAgents && room && <AgentLimits room={room} />}
    </aside>
  );
}
