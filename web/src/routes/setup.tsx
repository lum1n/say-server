import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Navigate, useNavigate } from "@tanstack/react-router";
import { getSetup, setup } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { Field, FormError } from "../components/form";

export const Route = createFileRoute("/setup")({
  validateSearch: (search: Record<string, unknown>): { code?: string } => ({
    code: typeof search.code === "string" ? search.code : undefined,
  }),
  component: SetupPage,
});

// SetupPage creates the first admin. The server logs a link here with the
// setup code filled in.
function SetupPage() {
  const search = Route.useSearch();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const status = useQuery({ queryKey: ["setup"], queryFn: () => getSetup() });
  const [code, setCode] = useState(search.code ?? "");
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const create = useMutation({
    mutationFn: () => setup({ code, username, display_name: displayName || undefined, password }),
    onSuccess: (me) => {
      queryClient.setQueryData(["me"], me);
      queryClient.setQueryData(["setup"], { needed: false });
      navigate({ to: "/" });
    },
  });

  // After our own setup succeeds, onSuccess navigates home instead.
  if (status.data && !status.data.needed && !create.isSuccess) return <Navigate to="/login" />;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <main className="auth">
      <h1>Set up say</h1>
      <p>Create the admin account. You can add people and agents afterwards.</p>
      <form onSubmit={submit}>
        {!search.code && (
          <Field label="Setup code" hint="Printed in the server's log at startup">
            <input value={code} onChange={(e) => setCode(e.target.value)} required autoComplete="off" />
          </Field>
        )}
        <Field label="Username">
          <input value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="username" />
        </Field>
        <Field label="Display name" hint="Optional; defaults to the username">
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} autoComplete="name" />
        </Field>
        <Field label="Password" hint="At least 8 characters">
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={8}
            autoComplete="new-password"
          />
        </Field>
        <FormError error={create.error} />
        <button type="submit" disabled={create.isPending}>
          Create admin
        </button>
      </form>
    </main>
  );
}
