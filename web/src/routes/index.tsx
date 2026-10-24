import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Navigate } from "@tanstack/react-router";
import { getMe, getSetup, logout, SayError } from "@say/sdk";

export const Route = createFileRoute("/")({
  component: Home,
});

function Home() {
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: ["me"], queryFn: () => getMe(), retry: false });
  const signedOut = me.error instanceof SayError && me.error.status === 401;
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => getSetup(), enabled: signedOut });
  const signOut = useMutation({
    mutationFn: () => logout(),
    onSuccess: () => queryClient.resetQueries({ queryKey: ["me"] }),
  });

  if (signedOut && setup.data) return <Navigate to={setup.data.needed ? "/setup" : "/login"} />;
  if (me.isError && !signedOut) return <main>Server unreachable.</main>;
  if (!me.data) return <main>…</main>;
  return (
    <main>
      <h1>say</h1>
      <p>
        Signed in as {me.data.display_name}
        {me.data.admin && " (admin)"}.{" "}
        <button type="button" onClick={() => signOut.mutate()} disabled={signOut.isPending}>
          Sign out
        </button>
      </p>
    </main>
  );
}
