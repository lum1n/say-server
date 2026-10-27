import { createFileRoute, Link } from "@tanstack/react-router";
import type { WidgetThread } from "@say/sdk";
import { useState } from "react";
import { useWidgetThreads } from "../../chat";
import { FormError } from "../../components/form";

// The inbox: widget threads by state, so visitors don't crowd the room
// list (docs/adr/0012-widget.md).
export const Route = createFileRoute("/_app/inbox")({
  component: Inbox,
});

type State = WidgetThread["state"];

const tabs: { state: State; label: string; empty: string }[] = [
  { state: "handed_over", label: "Handed over", empty: "No visitor is waiting for a person." },
  { state: "open", label: "Open", empty: "No agent is talking with a visitor." },
  { state: "idle", label: "Idle", empty: "No idle threads." },
];

function Inbox() {
  const threads = useWidgetThreads();
  const [picked, setPicked] = useState<State | null>(null);
  const list = threads.data ?? [];
  const count = (s: State) => list.filter((t) => t.state === s).length;
  // Until one is picked, show the first tab with threads in it.
  const state = picked ?? tabs.find((t) => count(t.state) > 0)?.state ?? "handed_over";
  const tab = tabs.find((t) => t.state === state)!;
  const shown = list.filter((t) => t.state === state);
  const widgets = new Set(list.map((t) => t.widget_id));
  // A visitor who signed in as someone with a thread already carries on
  // there; their anonymous thread says so, and theirs counts the others.
  const byVisitor = new Map(list.map((t) => [t.visitor_id, t]));
  const earlier = (t: WidgetThread) => list.filter((o) => o.signed_in_as?.visitor_id === t.visitor_id).length;
  return (
    <main className="page inbox">
      <Link to="/" className="back">‹ Rooms</Link>
      <h1>Inbox</h1>
      <p className="muted">
        Chats with visitors to your widgets. Open threads are with an agent; handed-over threads are the team's. A thread
        goes idle after 30 minutes without a message, unless its visitor is waiting for a person.
      </p>
      <div className="tabs" role="group" aria-label="Thread state">
        {tabs.map((t) => (
          <button key={t.state} type="button" aria-pressed={t.state === state} onClick={() => setPicked(t.state)}>
            {t.label} <span className="count">{count(t.state)}</span>
          </button>
        ))}
      </div>
      {threads.error && <FormError error={threads.error} />}
      <ul className="threads">
        {shown.map((t) => (
          <li key={t.room_id}>
            <Link to="/rooms/$roomId" params={{ roomId: t.room_id }}>
              <span className="who">
                {t.visitor_name}
                {t.verified && <span className="badge" title="Signed in on the host site">signed in</span>}
                {t.signed_in_as && (
                  <span className="badge" title="Signed in on the host site and carried on in their own thread">
                    then signed in{byVisitor.has(t.signed_in_as.visitor_id) && ` as ${byVisitor.get(t.signed_in_as.visitor_id)!.visitor_name}`}
                  </span>
                )}
                {t.verified && earlier(t) > 0 && (
                  <span className="badge" title="Threads they started before signing in">
                    {earlier(t) === 1 ? "1 earlier chat" : `${earlier(t)} earlier chats`}
                  </span>
                )}
                {t.waiting && <span className="badge waiting">waiting</span>}
              </span>
              {widgets.size > 1 && <span className="muted">{t.widget_name}</span>}
              <Ago at={t.last_message_at ?? t.created_at} />
            </Link>
          </li>
        ))}
        {threads.data && shown.length === 0 && <li className="muted">{tab.empty}</li>}
      </ul>
    </main>
  );
}

const ago = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

/** Ago says roughly how long ago at was, like "5 minutes ago". */
function Ago({ at }: { at: string }) {
  const s = (Date.parse(at) - Date.now()) / 1000;
  const [n, unit]: [number, Intl.RelativeTimeFormatUnit] =
    s > -60 ? [0, "second"] : s > -3600 ? [s / 60, "minute"] : s > -86400 ? [s / 3600, "hour"] : [s / 86400, "day"];
  return (
    <time className="muted" dateTime={at} title={new Date(at).toLocaleString()}>
      {n === 0 ? "just now" : ago.format(Math.round(n), unit)}
    </time>
  );
}
