import { type QueryClient, useQuery } from "@tanstack/react-query";
import { type Delivery, type Event, listPrincipals, listWidgetThreads, type Principal, Realtime, type WidgetThread } from "@say/sdk";
import { createContext, type ReactNode, useContext, useEffect, useState } from "react";

// Query keys shared by the chat routes.
export const keys = {
  me: ["me"] as const,
  rooms: ["rooms"] as const,
  principals: ["principals"] as const,
  builtinAgents: ["builtin-agents"] as const,
  widgets: ["widgets"] as const,
  widgetThreads: ["widget-threads"] as const,
  bridges: ["bridges"] as const,
  networks: ["networks"] as const,
  room: (id: string) => ["room", id] as const,
  members: (id: string) => ["members", id] as const,
  events: (id: string) => ["events", id] as const,
  deliveries: (id: string) => ["deliveries", id] as const,
};

/** Deliveries are how a room's messages went out to its bridges, by event id. */
export type Deliveries = Record<string, Delivery[]>;

/** mergeDeliveries replaces the deliveries of the events they name. */
export function mergeDeliveries(map: Deliveries | undefined, list: Delivery[], eventIds?: string[]): Deliveries {
  const next = { ...map };
  for (const id of eventIds ?? []) delete next[id];
  for (const d of list) next[d.event_id] = [...(next[d.event_id] ?? []).filter((x) => x.bridge_id !== d.bridge_id), d];
  return next;
}

/**
 * mergeEvents adds events to a room's log, keeping seq order without
 * duplicates. Messages read over HTTP already show their latest parts, so
 * edits in a page are kept for the cursor but not applied again.
 */
export function mergeEvents(log: Event[] | undefined, events: Event[]): Event[] {
  const bySeq = new Map((log ?? []).map((e) => [e.seq, e]));
  for (const e of events) bySeq.set(e.seq, e);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

/**
 * applyLive adds an event pushed by the socket, which arrive in seq order:
 * an edit also changes the message it names. Ending a stream makes the
 * message final; a later edit marks it edited.
 */
export function applyLive(log: Event[] | undefined, e: Event): Event[] {
  let next = log;
  if (e.type === "edit" && e.edits && log) {
    next = log.map((m) =>
      m.id !== e.edits
        ? m
        : e.stream === "final"
          ? { ...m, parts: e.parts, stream: "final" }
          : { ...m, parts: e.parts, edited_at: e.created_at },
    );
  }
  return mergeEvents(next, [e]);
}

/** applyStream shows a streaming message's latest parts, until it is final. */
export function applyStream(log: Event[] | undefined, msg: Event): Event[] | undefined {
  if (!log?.some((m) => m.id === msg.id && m.stream === "streaming")) return log;
  return log.map((m) => (m.id === msg.id ? { ...m, parts: msg.parts } : m));
}

const RealtimeContext = createContext<Realtime | null>(null);

/**
 * RealtimeProvider opens one socket for the signed-in app and pushes every
 * event it receives, and every change to a streaming message, into that
 * room's ["events", id] cache, and how messages went out to bridges into
 * ["deliveries", id]. It also rereads the room list and bridges when the
 * server says they changed.
 */
export function RealtimeProvider({ queryClient, children }: { queryClient: QueryClient; children: ReactNode }) {
  const [rt, setRt] = useState<Realtime | null>(null);
  useEffect(() => {
    const r = new Realtime({
      onEvent: (e) => queryClient.setQueryData<Event[]>(keys.events(e.room_id), (log) => applyLive(log, e)),
      onStream: (m) => queryClient.setQueryData<Event[]>(keys.events(m.room_id), (log) => applyStream(log, m)),
      onDelivery: (p) =>
        queryClient.setQueryData<Deliveries>(keys.deliveries(p.room_id), (map) => mergeDeliveries(map, p.deliveries, [p.event_id])),
      // A widget thread has a new message.
      onInboxChanged: () => queryClient.invalidateQueries({ queryKey: keys.widgetThreads }),
      onRoomsChanged: () => {
        queryClient.invalidateQueries({ queryKey: keys.rooms });
        // A widget thread was created or handed over.
        queryClient.invalidateQueries({ queryKey: keys.widgetThreads });
        // A bridge of mine or of a room changed state.
        queryClient.invalidateQueries({ queryKey: keys.bridges });
        // A room's agent settings changed, or its membership did.
        queryClient.invalidateQueries({ queryKey: ["room"] });
        queryClient.invalidateQueries({ queryKey: ["members"] });
      },
    });
    setRt(r);
    return () => r.close();
  }, [queryClient]);
  return <RealtimeContext.Provider value={rt}>{children}</RealtimeContext.Provider>;
}

export function useRealtime() {
  return useContext(RealtimeContext);
}

/**
 * useDirectory maps principal ids to principals. It is refetched every
 * 30 s, because whether a bot is online is not pushed.
 */
export function useDirectory() {
  const q = useQuery({
    queryKey: keys.principals,
    queryFn: () => listPrincipals(),
    staleTime: 30_000,
    refetchInterval: 30_000,
  });
  const byId = new Map<string, Principal>((q.data ?? []).map((p) => [p.id, p]));
  return { byId, list: q.data ?? [], refetch: q.refetch, isFetching: q.isFetching };
}

/**
 * useWidgetThreads reads the inbox. The socket says when a thread has a
 * new message (RealtimeProvider); a thread going idle is not pushed, so
 * the inbox is reread when the next one does.
 */
export function useWidgetThreads(enabled = true) {
  return useQuery({
    queryKey: keys.widgetThreads,
    queryFn: () => listWidgetThreads(),
    enabled,
    staleTime: 15_000,
    refetchInterval: (q) => untilIdle(q.state.data, Date.now()),
  });
}

/** untilIdle is how long until the next of threads goes idle, if any will. */
export function untilIdle(threads: WidgetThread[] | undefined, now: number): number | false {
  let next = Infinity;
  for (const t of threads ?? []) {
    if (t.state !== "idle" && t.idle_at) next = Math.min(next, Date.parse(t.idle_at));
  }
  // A second late, so the server agrees the thread is idle.
  return next === Infinity ? false : Math.max(next - now, 0) + 1_000;
}

/** Online marks an agent whose bot is connected. */
export function Online({ principal: p }: { principal?: Principal }) {
  const c = p?.connector;
  if (!c) return null;
  const what = [c.name, c.version].filter(Boolean).join(" ");
  return (
    <span className="online" title={what ? `Connected: ${what}` : "Connected"}>
      online
    </span>
  );
}
