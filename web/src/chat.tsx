import { type QueryClient, useQuery } from "@tanstack/react-query";
import { type Event, listPrincipals, type Principal, Realtime } from "@say/sdk";
import { createContext, type ReactNode, useContext, useEffect, useState } from "react";

// Query keys shared by the chat routes.
export const keys = {
  me: ["me"] as const,
  rooms: ["rooms"] as const,
  principals: ["principals"] as const,
  room: (id: string) => ["room", id] as const,
  members: (id: string) => ["members", id] as const,
  events: (id: string) => ["events", id] as const,
};

/** mergeEvents adds events to a room's log, keeping seq order without duplicates. */
export function mergeEvents(log: Event[] | undefined, events: Event[]): Event[] {
  const bySeq = new Map((log ?? []).map((e) => [e.seq, e]));
  for (const e of events) bySeq.set(e.seq, e);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

const RealtimeContext = createContext<Realtime | null>(null);

/**
 * RealtimeProvider opens one socket for the signed-in app and pushes every
 * event it receives into that room's ["events", id] cache. It also
 * rereads the room list when the server says it changed.
 */
export function RealtimeProvider({ queryClient, children }: { queryClient: QueryClient; children: ReactNode }) {
  const [rt, setRt] = useState<Realtime | null>(null);
  useEffect(() => {
    const r = new Realtime({
      onEvent: (e) => queryClient.setQueryData<Event[]>(keys.events(e.room_id), (log) => mergeEvents(log, [e])),
      onRoomsChanged: () => queryClient.invalidateQueries({ queryKey: keys.rooms }),
    });
    setRt(r);
    return () => r.close();
  }, [queryClient]);
  return <RealtimeContext.Provider value={rt}>{children}</RealtimeContext.Provider>;
}

export function useRealtime() {
  return useContext(RealtimeContext);
}

/** useDirectory maps principal ids to principals. */
export function useDirectory() {
  const q = useQuery({ queryKey: keys.principals, queryFn: () => listPrincipals(), staleTime: 60_000 });
  const byId = new Map<string, Principal>((q.data ?? []).map((p) => [p.id, p]));
  return { byId, list: q.data ?? [], refetch: q.refetch, isFetching: q.isFetching };
}
