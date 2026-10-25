import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { login } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { Field, FormError } from "../components/form";

export const Route = createFileRoute("/login")({
  component: LoginPage,
});

function LoginPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const signIn = useMutation({
    mutationFn: () => login({ username, password }),
    onSuccess: (me) => {
      queryClient.setQueryData(["me"], me);
      navigate({ to: "/" });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    signIn.mutate();
  };
  return (
    <main className="auth">
      <h1>Sign in</h1>
      <form onSubmit={submit}>
        <Field label="Username">
          <input value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="username" />
        </Field>
        <Field label="Password">
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            autoComplete="current-password"
          />
        </Field>
        <FormError error={signIn.error} />
        <button type="submit" disabled={signIn.isPending}>
          Sign in
        </button>
      </form>
    </main>
  );
}
