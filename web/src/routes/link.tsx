import { useQuery } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { getBridgeScreen, SayError } from "@say/sdk";
import { type FormEvent, useState } from "react";
import { renderSVG } from "uqr";

// Shows a network's login step, such as Signal's QR code, on this screen
// for a code the say app made (POST /bridges/{id}/screen): the phone that
// must scan it is often the one the person uses say on. No sign-in here.
export const Route = createFileRoute("/link")({
  component: LinkPage,
  validateSearch: (s: Record<string, unknown>) => ({ code: typeof s.code === "string" ? s.code : "" }),
});

function LinkPage() {
  const { code } = Route.useSearch();
  return <main className="auth link">{code ? <Screen code={code} /> : <EnterCode />}</main>;
}

function EnterCode() {
  const navigate = useNavigate();
  const [value, setValue] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    navigate({ to: "/link", search: { code: value.trim() } });
  };
  return (
    <form onSubmit={submit}>
      <h1>Show a code from say</h1>
      <p>Type the code the say app shows under "Show the code on another screen".</p>
      <input
        className="code"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        placeholder="K3X9Q-2M7TA"
        autoCapitalize="characters"
        autoComplete="off"
        autoFocus
        required
      />
      <p>
        <button type="submit">Show</button>
      </p>
    </form>
  );
}

const steps: Record<string, string[]> = {
  signal: ["Open Signal on your phone", "Go to Settings, then Linked devices", "Tap Link new device and scan this code"],
};

function Screen({ code }: { code: string }) {
  const screen = useQuery({
    queryKey: ["screen", code],
    queryFn: () => getBridgeScreen(code),
    // Follows the bridge: a fresh QR code, or the moment it's connected.
    refetchInterval: (q) => (q.state.data?.state === "live" || q.state.error ? false : 2000),
    retry: (n, err) => !(err instanceof SayError && err.status === 404) && n < 3,
  });
  if (screen.error) {
    const gone = screen.error instanceof SayError && screen.error.status === 404;
    return (
      <>
        <h1>{gone ? "This code has run out" : "Your say can't be reached"}</h1>
        <p>{gone ? "Make a new one in the say app, under Networks." : "Check that it's running, then reload this page."}</p>
      </>
    );
  }
  const s = screen.data;
  if (!s) return <p className="muted">Loading…</p>;
  if (s.state === "live") {
    return (
      <>
        <h1>{s.title} is connected</h1>
        <p>Your chats are on their way to the say app. You can close this page.</p>
      </>
    );
  }
  const p = s.prompt;
  return (
    <>
      <h1>Connect {s.title}</h1>
      {p?.kind === "qr" && p.data ? (
        <>
          <ol>
            {(steps[s.network] ?? [p.text]).map((step) => (
              <li key={step}>{step}</li>
            ))}
          </ol>
          <div className="qr" role="img" aria-label="QR code" dangerouslySetInnerHTML={{ __html: renderSVG(p.data) }} />
          <p className="muted">The code changes every few minutes; this page keeps up.</p>
        </>
      ) : p ? (
        <p>{s.title} asks for something only the say app can answer: carry on there.</p>
      ) : (
        <p className="muted">Your say is talking to {s.title}…</p>
      )}
    </>
  );
}
