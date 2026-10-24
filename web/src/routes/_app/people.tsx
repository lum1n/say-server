import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { createPrincipal, type CreatedPrincipal } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { keys, useDirectory } from "../../chat";
import { Field, FormError } from "../../components/form";

// People is where instance admins add people (who sign in with a password)
// and agents (which get a token).
export const Route = createFileRoute("/_app/people")({
  component: People,
});

function People() {
  const dir = useDirectory();
  return (
    <main className="page">
      <Link to="/" className="back">‹ Rooms</Link>
      <h1>People and agents</h1>
      <ul className="directory">
        {dir.list.map((p) => (
          <li key={p.id}>
            {p.display_name} <span className="muted">{p.kind}</span>
            {p.admin && <span className="badge">admin</span>}
          </li>
        ))}
      </ul>
      <AddPerson />
      <AddAgent />
    </main>
  );
}

function AddPerson() {
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const add = useMutation({
    mutationFn: () =>
      createPrincipal({ kind: "human", username, display_name: displayName || undefined, password }),
    onSuccess: () => {
      setUsername("");
      setDisplayName("");
      setPassword("");
      queryClient.invalidateQueries({ queryKey: keys.principals });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };
  return (
    <form onSubmit={submit}>
      <h2>Add a person</h2>
      <Field label="Username">
        <input value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="off" />
      </Field>
      <Field label="Display name" hint="Optional; defaults to the username">
        <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} autoComplete="off" />
      </Field>
      <Field label="Password" hint="At least 8 characters; give it to them yourself">
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          minLength={8}
          autoComplete="new-password"
        />
      </Field>
      <FormError error={add.error} />
      {add.isSuccess && <p role="status">Added {add.data.principal.display_name}.</p>}
      <button type="submit" disabled={add.isPending}>
        Add person
      </button>
    </form>
  );
}

function AddAgent() {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<CreatedPrincipal | null>(null);
  const add = useMutation({
    mutationFn: () => createPrincipal({ kind: "agent", display_name: name }),
    onSuccess: (c) => {
      setName("");
      setCreated(c);
      queryClient.invalidateQueries({ queryKey: keys.principals });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };
  return (
    <form onSubmit={submit}>
      <h2>Add an agent</h2>
      <Field label="Name">
        <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoComplete="off" />
      </Field>
      <FormError error={add.error} />
      {created?.token && (
        <div role="status" className="token">
          <p>
            Token for {created.principal.display_name}. Copy it now: it is not shown again.
          </p>
          <code>{created.token}</code>
        </div>
      )}
      <button type="submit" disabled={add.isPending}>
        Add agent
      </button>
    </form>
  );
}
