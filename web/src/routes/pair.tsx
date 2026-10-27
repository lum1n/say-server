import { createFileRoute } from "@tanstack/react-router";

// The page an "Add a device" QR code opens when a phone's camera reads it
// without the say app: the code is for the app (docs/adr/0018-devices.md).
export const Route = createFileRoute("/pair")({
  component: Pair,
  validateSearch: (s: Record<string, unknown>) => ({ code: typeof s.code === "string" ? s.code : "" }),
});

function Pair() {
  const { code } = Route.useSearch();
  return (
    <main className="auth">
      <h1>Add a device</h1>
      <p>Open the say app on this device and scan the code again, or choose to sign in with a code and enter:</p>
      <p>
        <code className="code">{code}</code>
      </p>
      <p className="muted">{location.origin}</p>
    </main>
  );
}
