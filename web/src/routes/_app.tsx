import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Navigate, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import { createRoom, getMe, getSetup, listRooms, logout, SayError } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { keys, RealtimeProvider } from "../chat";
import { FormError } from "../components/form";

// The signed-in app: the room list beside the open room. Signed-out
// visitors go to setup or sign-in.
export const Route = createFileRoute("/_app")({
  component: AppLayout,
});

function AppLayout() {
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: keys.me, queryFn: () => getMe(), retry: false });
  const signedOut = me.error instanceof SayError && me.error.status === 401;
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => getSetup(), enabled: signedOut });
  // Phones show the room list or the open page, not both (styles.css).
  const view = useRouterState({ select: (s) => (s.location.pathname === "/" ? "list" : "page") });

  if (signedOut && setup.data) return <Navigate to={setup.data.needed ? "/setup" : "/login"} />;
  if (me.isError && !signedOut) return <main className="center">Server unreachable.</main>;
  if (!me.data) return <main className="center">…</main>;
  return (
    <RealtimeProvider queryClient={queryClient}>
      <div className="app" data-view={view}>
        <Sidebar admin={!!me.data.admin} name={me.data.display_name} />
        <Outlet />
      </div>
    </RealtimeProvider>
  );
}

function Sidebar({ admin, name }: { admin: boolean; name: string }) {
  const queryClient = useQueryClient();
  // The socket says when this list changes (RealtimeProvider).
  const rooms = useQuery({ queryKey: keys.rooms, queryFn: () => listRooms() });
  const signOut = useMutation({
    mutationFn: () => logout(),
    onSuccess: () => {
      queryClient.clear();
      queryClient.resetQueries({ queryKey: keys.me });
    },
  });
  return (
    <nav className="sidebar" aria-label="Rooms">
      <header>
        <strong>say</strong>
      </header>
      <ul className="rooms">
        {rooms.data?.map((r) => (
          <li key={r.id}>
            <Link to="/rooms/$roomId" params={{ roomId: r.id }} activeProps={{ "aria-current": "page" }}>
              {r.kind === "channel" ? "# " : ""}
              {r.name || "Untitled room"}
            </Link>
          </li>
        ))}
        {rooms.data?.length === 0 && <li className="muted">No rooms yet</li>}
      </ul>
      <NewRoom />
      <footer>
        {admin && <Link to="/people">People and agents</Link>}
        <span>
          {name}{" "}
          <button type="button" className="link" onClick={() => signOut.mutate()} disabled={signOut.isPending}>
            Sign out
          </button>
        </span>
      </footer>
    </nav>
  );
}

function NewRoom() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [kind, setKind] = useState<"group" | "channel">("group");
  const create = useMutation({
    mutationFn: () => createRoom({ kind, name }),
    onSuccess: (room) => {
      setName("");
      queryClient.invalidateQueries({ queryKey: keys.rooms });
      navigate({ to: "/rooms/$roomId", params: { roomId: room.id } });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <form className="new-room" onSubmit={submit}>
      <input aria-label="New room name" placeholder="New room" value={name} onChange={(e) => setName(e.target.value)} required maxLength={200} />
      <select aria-label="Room kind" value={kind} onChange={(e) => setKind(e.target.value as "group" | "channel")}>
        <option value="group">Group</option>
        <option value="channel">Channel</option>
      </select>
      <button type="submit" disabled={create.isPending}>
        Create
      </button>
      <FormError error={create.error} />
    </form>
  );
}
