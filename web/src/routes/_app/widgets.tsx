import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { createWidget, deleteWidget, listWidgets, updateWidget, type Widget, type WidgetSettings } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { keys, useDirectory } from "../../chat";
import { Field, FormError } from "../../components/form";

// Widgets is where instance admins put a chat on other sites: each widget
// is a <say-chat> snippet, the origins that may embed it and who answers
// its visitors (docs/adr/0012-widget.md).
export const Route = createFileRoute("/_app/widgets")({
  component: Widgets,
});

function Widgets() {
  const widgets = useQuery({ queryKey: keys.widgets, queryFn: () => listWidgets() });
  return (
    <main className="page">
      <Link to="/" className="back">‹ Rooms</Link>
      <h1>Widgets</h1>
      <p className="muted">
        A widget puts a chat on your own site. Each visitor gets a thread with the widget's agent and team, which shows up
        in the team's room list. When a person on the team writes in a thread, the agent stops answering it.
      </p>
      {widgets.error && <FormError error={widgets.error} />}
      <ul className="directory widgets">
        {widgets.data?.map((w) => <WidgetRow key={w.id} widget={w} />)}
      </ul>
      <WidgetForm />
    </main>
  );
}

function snippet(w: Widget) {
  return `<script src="${location.origin}/widget.js" async></script>\n<say-chat widget="${w.public_key}"></say-chat>`;
}

function WidgetRow({ widget: w }: { widget: Widget }) {
  const dir = useDirectory();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState(false);
  const rotate = useMutation({
    mutationFn: () => updateWidget(w.id, { rotate_secret: true }),
    onSuccess: (nw) => queryClient.setQueryData<Widget[]>(keys.widgets, (l) => l?.map((x) => (x.id === nw.id ? nw : x))),
  });
  const toggle = useMutation({
    mutationFn: () => updateWidget(w.id, { enabled: !w.enabled }),
    onSuccess: (nw) => queryClient.setQueryData<Widget[]>(keys.widgets, (l) => l?.map((x) => (x.id === nw.id ? nw : x))),
  });
  const remove = useMutation({
    mutationFn: () => deleteWidget(w.id),
    onSuccess: () => queryClient.setQueryData<Widget[]>(keys.widgets, (l) => l?.filter((x) => x.id !== w.id)),
  });
  const answers = [
    w.agent_id && `${dir.byId.get(w.agent_id)?.display_name ?? "an agent"} (agent)`,
    ...w.team.map((id) => dir.byId.get(id)?.display_name ?? "someone"),
  ].filter(Boolean);
  return (
    <li>
      <div>
        <strong>{w.name}</strong> <span className="muted">on {w.origins.join(", ")}</span>{" "}
        <button type="button" className="link" onClick={() => setEditing(!editing)}>
          {editing ? "Close" : "Edit"}
        </button>{" "}
        <button type="button" className="link" onClick={() => toggle.mutate()} disabled={toggle.isPending}>
          {w.enabled ? "Switch off" : "Switch on"}
        </button>{" "}
        <button
          type="button"
          className="link"
          disabled={remove.isPending}
          onClick={() =>
            confirm(`Delete ${w.name}? It disappears from your sites and its visitors are signed out. Their threads stay.`) &&
            remove.mutate()
          }
        >
          Delete
        </button>
      </div>
      {!w.enabled && <p className="muted">Switched off: your sites say the chat is not available, and visitors cannot write.</p>}
      <p className="muted">Answered by {answers.join(", ")}</p>
      <FormError error={toggle.error ?? remove.error} />
      <label className="snippet">
        Paste into your site's HTML
        <textarea readOnly rows={2} value={snippet(w)} onFocus={(e) => e.currentTarget.select()} />
      </label>
      <details>
        <summary>Signed-in visitors</summary>
        <p className="muted">
          To show your signed-in users as themselves, sign a JWT on your server with HS256 and this secret, with{" "}
          <code>sub</code> (your user id), <code>exp</code> (at most 24 hours ahead) and optionally <code>name</code>, and set
          it as the <code>identity</code> attribute of <code>&lt;say-chat&gt;</code>. Keep the secret on your server.
        </p>
        <code className="secret">{w.secret}</code>{" "}
        <button
          type="button"
          className="link"
          disabled={rotate.isPending}
          onClick={() => confirm("Replace the secret? Tokens signed with the old one stop working.") && rotate.mutate()}
        >
          Replace secret
        </button>
        <FormError error={rotate.error} />
      </details>
      <a href={`/embed/${w.public_key}`} target="_blank" rel="noreferrer">
        Try it
      </a>
      {editing && <WidgetForm widget={w} onSaved={() => setEditing(false)} />}
    </li>
  );
}

function WidgetForm({ widget, onSaved }: { widget?: Widget; onSaved?: () => void }) {
  const queryClient = useQueryClient();
  const dir = useDirectory();
  const [name, setName] = useState(widget?.name ?? "");
  const [origins, setOrigins] = useState(widget?.origins.join("\n") ?? "");
  const [agentId, setAgentId] = useState(widget?.agent_id ?? "");
  const [team, setTeam] = useState<string[]>(widget?.team ?? []);
  const [greeting, setGreeting] = useState(widget?.greeting ?? "");
  const save = useMutation({
    mutationFn: () => {
      const body: WidgetSettings = {
        name,
        origins: origins.split(/[\s,]+/).filter(Boolean),
        agent_id: agentId,
        team,
        greeting,
      };
      return widget ? updateWidget(widget.id, body) : createWidget(body);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: keys.widgets });
      if (!widget) {
        setName("");
        setOrigins("");
        setAgentId("");
        setTeam([]);
        setGreeting("");
      }
      onSaved?.();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate();
  };
  const agents = dir.list.filter((p) => p.kind === "agent");
  const people = dir.list.filter((p) => p.kind === "human");
  return (
    <form onSubmit={submit}>
      {!widget && <h2>Add a widget</h2>}
      <Field label="Name" hint="Shown at the top of the chat">
        <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoComplete="off" />
      </Field>
      <Field label="Sites" hint="Origins that may embed it, one per line, like https://shop.example">
        <textarea value={origins} onChange={(e) => setOrigins(e.target.value)} required rows={2} />
      </Field>
      <Field label="Agent" hint="Answers every visitor until a person takes over">
        <select value={agentId} onChange={(e) => setAgentId(e.target.value)}>
          <option value="">None</option>
          {agents.map((a) => (
            <option key={a.id} value={a.id}>
              {a.display_name}
            </option>
          ))}
        </select>
      </Field>
      <fieldset className="team">
        <legend>Team</legend>
        <small className="muted">People added to every new thread</small>
        {people.map((p) => (
          <label key={p.id}>
            <input
              type="checkbox"
              checked={team.includes(p.id)}
              onChange={(e) => setTeam(e.target.checked ? [...team, p.id] : team.filter((id) => id !== p.id))}
            />{" "}
            {p.display_name}
          </label>
        ))}
      </fieldset>
      <Field label="Greeting" hint="Optional; the first thing visitors see">
        <textarea value={greeting} onChange={(e) => setGreeting(e.target.value)} rows={2} maxLength={1000} />
      </Field>
      <FormError error={save.error} />
      <button type="submit" disabled={save.isPending}>
        {widget ? "Save" : "Add widget"}
      </button>
    </form>
  );
}
