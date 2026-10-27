import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { createPairing, getMe, listTokens, revokeToken, type Token } from "@say/sdk";
import { useEffect, useState } from "react";
import { renderSVG } from "uqr";
import { keys } from "../../chat";
import { FormError } from "../../components/form";

// Devices is where people see what is signed in as them, sign a lost
// phone out, and add a device with a one-time code (docs/adr/0018-devices.md).
export const Route = createFileRoute("/_app/devices")({
  component: Devices,
});

function Devices() {
  const me = useQuery({ queryKey: keys.me, queryFn: () => getMe() });
  const id = me.data?.id ?? "";
  const tokens = useQuery({ queryKey: keys.tokens(id), queryFn: () => listTokens(id), enabled: !!id });
  return (
    <main className="page devices">
      <Link to="/" className="back">‹ Rooms</Link>
      <h1>Devices</h1>
      <p className="muted">
        Everything signed in as you. Sign out one you've lost or no longer use; it is cut off within half a minute.
      </p>
      {tokens.error && <FormError error={tokens.error} />}
      <ul className="directory">
        {tokens.data?.map((t) => <TokenRow key={t.id} principalId={id} token={t} />)}
      </ul>
      <AddDevice />
    </main>
  );
}

function TokenRow({ principalId, token: t }: { principalId: string; token: Token }) {
  const queryClient = useQueryClient();
  const revoke = useMutation({
    mutationFn: () => revokeToken(principalId, t.id),
    onSuccess: () => {
      if (t.current) {
        location.assign("/login");
        return;
      }
      queryClient.invalidateQueries({ queryKey: keys.tokens(principalId) });
    },
  });
  const signOut = () => {
    const what = t.current ? "This signs this browser out." : `Sign out ${describe(t)}?`;
    if (confirm(what)) revoke.mutate();
  };
  return (
    <li>
      <span>
        {describe(t)} {t.current && <span className="badge">this one</span>}
        <br />
        <span className="muted">
          {t.last_used_at ? `active ${ago(t.last_used_at)}` : `added ${ago(t.created_at)}`}
          {t.expires_at && `, until ${new Date(t.expires_at).toLocaleDateString()}`}
        </span>
      </span>{" "}
      <button type="button" className="link" onClick={signOut} disabled={revoke.isPending}>
        Sign out
      </button>
      <FormError error={revoke.error} />
    </li>
  );
}

// describe names a token for people: the device's own name, or the
// browser and system it signed in from.
function describe(t: Token): string {
  if (t.name.startsWith("device:")) return t.name.slice("device:".length);
  if (t.name === "session") return client(t.user_agent) ?? t.user_agent?.split(" ")[0] ?? "A browser";
  if (t.name === "cli") return "say on the command line";
  return t.name;
}

function client(ua?: string): string | undefined {
  if (!ua) return undefined;
  const browser = [
    ["Edg/", "Edge"],
    ["Firefox/", "Firefox"],
    ["Chrome/", "Chrome"],
    ["Safari/", "Safari"],
  ].find(([k]) => ua.includes(k))?.[1];
  const os = [
    ["iPhone", "iPhone"],
    ["iPad", "iPad"],
    ["Android", "Android"],
    ["Mac OS X", "macOS"],
    ["Windows", "Windows"],
    ["Linux", "Linux"],
  ].find(([k]) => ua.includes(k))?.[1];
  if (!browser) return undefined;
  return os ? `${browser} on ${os}` : browser;
}

function ago(at: string): string {
  const s = (Date.now() - new Date(at).getTime()) / 1000;
  if (s < 3600) return "within the hour";
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} days ago`;
}

// AddDevice shows a one-time code as a QR code for the say app to scan,
// and as text to type in.
function AddDevice() {
  const pairing = useMutation({ mutationFn: () => createPairing() });
  const left = useCountdown(pairing.data?.expires_at);
  const p = pairing.data;
  const link = p && `${location.origin}/pair?code=${encodeURIComponent(p.code)}`;
  return (
    <section>
      <h2>Add a device</h2>
      {p && link && left > 0 ? (
        <div className="pairing">
          <p>In the say app on the new device, scan this code, or enter this node's address and the code.</p>
          <div className="qr" role="img" aria-label="QR code" dangerouslySetInnerHTML={{ __html: renderSVG(link) }} />
          <p>
            <code className="code">{p.code}</code>
            <br />
            <span className="muted">
              {location.origin} · works once, for {Math.ceil(left / 60)} more min
            </span>
          </p>
        </div>
      ) : (
        <button type="button" onClick={() => pairing.mutate()} disabled={pairing.isPending}>
          {p ? "Make a new code" : "Show a code"}
        </button>
      )}
      <FormError error={pairing.error} />
    </section>
  );
}

// useCountdown is the seconds left until at, ticking.
function useCountdown(at?: string): number {
  const until = at ? new Date(at).getTime() : 0;
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!until) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [until]);
  return Math.max(0, Math.floor((until - now) / 1000));
}
