import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/_app/")({
  component: () => <main className="empty muted">Pick a room, or create one.</main>,
});
