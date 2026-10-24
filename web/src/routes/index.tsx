import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { getHealth } from "@say/sdk";

export const Route = createFileRoute("/")({
  component: Home,
});

function Home() {
  const health = useQuery({ queryKey: ["health"], queryFn: () => getHealth() });
  return (
    <main>
      <h1>say</h1>
      <p>Server: {health.data?.status ?? (health.isError ? "unreachable" : "…")}</p>
    </main>
  );
}
