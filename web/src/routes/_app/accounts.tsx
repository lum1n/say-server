import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  answerBridge,
  type Bridge,
  type BridgeMode,
  createBridge,
  createBridgeLink,
  getMe,
  listBridgeLinks,
  listBridges,
  listNetworks,
  listRooms,
  type Network,
  type NetworkField,
  updateBridge,
} from "@say/sdk";
import { type FormEvent, useState } from "react";
import { renderSVG } from "uqr";
import { keys } from "../../chat";
import { Field, FormError } from "../../components/form";

// Accounts is where people connect their own accounts on other networks:
// each chat there becomes a room here, and what they write in it goes out
// as them. Instance admins also add relay bots here, which mirror rooms
// they link (docs/adr/0014-bridges.md).
export const Route = createFileRoute("/_app/accounts")({
  component: Accounts,
});

function Accounts() {
  const bridges = useQuery({ queryKey: keys.bridges, queryFn: () => listBridges() });
  const networks = useQuery({ queryKey: keys.networks, queryFn: () => listNetworks(), staleTime: Infinity });
  const byName = new Map((networks.data ?? []).map((n) => [n.name, n]));
  return (
    <main className="page accounts">
      <Link to="/" className="back">‹ Rooms</Link>
      <h1>Accounts</h1>
      <p className="muted">
        Connect your accounts on other networks. Your chats there show up as rooms here, and what you write in them is sent
        as you. Nobody else in those rooms, people or agents, can write as you. The server reads these messages; they are
        not end-to-end encrypted.
      </p>
      {bridges.error && <FormError error={bridges.error} />}
      <ul className="directory">
        {bridges.data?.map((b) => <BridgeRow key={b.id} bridge={b} network={byName.get(b.network)} />)}
      </ul>
      {networks.data && <AddAccount networks={networks.data} />}
    </main>
  );
}

const stateText: Record<string, string> = {
  live: "connected",
  login: "waiting for you",
  error: "can't connect",
  offline: "offline",
  disabled: "switched off",
};

function BridgeRow({ bridge: b, network }: { bridge: Bridge; network?: Network }) {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState(false);
  const toggle = useMutation({
    mutationFn: () => updateBridge(b.id, { enabled: !b.enabled }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: keys.bridges }),
  });
  return (
    <li>
      <div>
        <strong>{b.name}</strong>
        {b.name !== (network?.title ?? b.network) && <span className="muted"> {network?.title ?? b.network}</span>}
        {b.mode === "bot" && <span className="badge">relay bot</span>}
        <span className={`state ${b.state}`}>{stateText[b.state] ?? b.state}</span>{" "}
        <button type="button" className="link" onClick={() => setEditing(!editing)}>
          {editing ? "Close" : "Settings"}
        </button>{" "}
        <button type="button" className="link" onClick={() => toggle.mutate()} disabled={toggle.isPending}>
          {b.enabled ? "Switch off" : "Switch on"}
        </button>
      </div>
      {b.error && b.state !== "live" && (
        <p role="alert" className="muted">
          {b.error}
        </p>
      )}
      {b.prompt && <PromptBox bridge={b} />}
      <FormError error={toggle.error} />
      {editing && network && <ConfigForm network={network} mode={b.mode} bridge={b} onSaved={() => setEditing(false)} />}
      <Chats bridge={b} />
    </li>
  );
}

// PromptBox shows a login step only the owner can do: scan a QR code, or
// type a code or password.
function PromptBox({ bridge: b }: { bridge: Bridge }) {
  const p = b.prompt!;
  const [value, setValue] = useState("");
  const answer = useMutation({
    mutationFn: () => answerBridge(b.id, value),
    onSuccess: () => setValue(""),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    answer.mutate();
  };
  if (p.kind === "qr") {
    return (
      <div className="prompt">
        <p>{p.text}</p>
        {p.data && <div className="qr" role="img" aria-label="QR code" dangerouslySetInnerHTML={{ __html: renderSVG(p.data) }} />}
      </div>
    );
  }
  return (
    <form className="prompt" onSubmit={submit}>
      <Field label={p.text || "Answer"}>
        <input
          type={p.kind === "password" ? "password" : "text"}
          inputMode={p.kind === "code" ? "numeric" : undefined}
          autoComplete={p.kind === "code" ? "one-time-code" : "off"}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          required
        />
      </Field>
      <FormError error={answer.error} />
      <button type="submit" disabled={answer.isPending}>
        Send
      </button>
    </form>
  );
}

// Chats lists the rooms a bridge mirrors. A relay bot's admin links more
// here.
function Chats({ bridge: b }: { bridge: Bridge }) {
  const [open, setOpen] = useState(false);
  const links = useQuery({ queryKey: ["bridge-links", b.id], queryFn: () => listBridgeLinks(b.id), enabled: open });
  return (
    <details onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>{b.mode === "bot" ? "Linked rooms" : "Chats"}</summary>
      {links.data?.length === 0 && (
        <p className="muted">{b.mode === "bot" ? "No rooms linked yet." : "No chats yet; they appear as messages arrive."}</p>
      )}
      <ul className="chats">
        {links.data?.map((l) => (
          <li key={l.id}>
            <Link to="/rooms/$roomId" params={{ roomId: l.room_id }}>
              {l.room_name || l.remote_name || l.remote_id}
            </Link>
            {b.mode === "bot" && <span className="muted"> ↔ {l.remote_name || l.remote_id}</span>}
          </li>
        ))}
      </ul>
      {open && b.mode === "bot" && <LinkRoom bridge={b} />}
    </details>
  );
}

function LinkRoom({ bridge: b }: { bridge: Bridge }) {
  const queryClient = useQueryClient();
  const rooms = useQuery({ queryKey: keys.rooms, queryFn: () => listRooms() });
  const [roomId, setRoomId] = useState("");
  const [remote, setRemote] = useState("");
  const link = useMutation({
    mutationFn: () => createBridgeLink(b.id, { room_id: roomId, remote_id: remote }),
    onSuccess: () => {
      setRemote("");
      queryClient.invalidateQueries({ queryKey: ["bridge-links", b.id] });
      queryClient.invalidateQueries({ queryKey: keys.rooms });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    link.mutate();
  };
  return (
    <form onSubmit={submit}>
      <select aria-label="Room" value={roomId} onChange={(e) => setRoomId(e.target.value)} required>
        <option value="">Room…</option>
        {rooms.data?.map((r) => (
          <option key={r.id} value={r.id}>
            {r.name || "Untitled room"}
          </option>
        ))}
      </select>
      <input aria-label="Remote channel" placeholder="Channel there" value={remote} onChange={(e) => setRemote(e.target.value)} required />
      <button type="submit" disabled={link.isPending}>
        Link
      </button>
      <FormError error={link.error} />
    </form>
  );
}

function AddAccount({ networks }: { networks: Network[] }) {
  const me = useQuery({ queryKey: keys.me, queryFn: () => getMe() });
  const [name, setName] = useState("");
  const [mode, setMode] = useState<BridgeMode>("account");
  const usable = networks.filter((n) => n.modes.includes("account") || (me.data?.admin && n.modes.includes("bot")));
  const network = usable.find((n) => n.name === name);
  const modes = network?.modes.filter((m) => m === "account" || me.data?.admin) ?? [];
  return (
    <section>
      <h2>Connect an account</h2>
      <Field label="Network">
        <select
          value={name}
          onChange={(e) => {
            setName(e.target.value);
            setMode("account");
          }}
        >
          <option value="">Choose…</option>
          {usable.map((n) => (
            <option key={n.name} value={n.name}>
              {n.title}
            </option>
          ))}
        </select>
      </Field>
      {modes.length > 1 && (
        <Field label="Kind" hint="A relay bot mirrors rooms you link to its channels, for everyone in them">
          <select value={mode} onChange={(e) => setMode(e.target.value as BridgeMode)}>
            <option value="account">My account</option>
            <option value="bot">Relay bot</option>
          </select>
        </Field>
      )}
      {network && (
        <ConfigForm key={`${network.name}-${mode}`} network={network} mode={modes.includes(mode) ? mode : "account"} onSaved={() => setName("")} />
      )}
    </section>
  );
}

// ConfigForm edits a bridge's settings, or makes a new bridge. Secrets
// are never shown; one left empty keeps its value.
function ConfigForm({
  network,
  mode,
  bridge,
  onSaved,
}: {
  network: Network;
  mode: BridgeMode;
  bridge?: Bridge;
  onSaved?: () => void;
}) {
  const queryClient = useQueryClient();
  const fields = network.fields.filter((f) => !f.modes || f.modes.includes(mode));
  const [name, setName] = useState(bridge?.name ?? network.title);
  const [values, setValues] = useState<Record<string, unknown>>(() => {
    const v: Record<string, unknown> = {};
    for (const f of fields) {
      const cur = bridge?.config[f.key];
      v[f.key] = f.kind === "bool" ? Boolean(cur) : f.kind === "secret" ? "" : cur === undefined ? "" : String(cur);
    }
    return v;
  });
  const save = useMutation({
    mutationFn: () => {
      const config: Record<string, unknown> = {};
      for (const f of fields) {
        const v = values[f.key];
        if (f.kind === "secret") {
          if (v) config[f.key] = v;
        } else if (f.kind === "number") {
          if (v !== "") config[f.key] = Number(v);
        } else if (f.kind === "bool") {
          config[f.key] = v;
        } else if (bridge || v !== "") {
          config[f.key] = v;
        }
      }
      return bridge ? updateBridge(bridge.id, { name, config }) : createBridge({ network: network.name, mode, name, config });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: keys.bridges });
      onSaved?.();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate();
  };
  const set = (key: string, v: unknown) => setValues((vs) => ({ ...vs, [key]: v }));
  return (
    <form onSubmit={submit}>
      <Field label="Name" hint="For you, in this list">
        <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoComplete="off" />
      </Field>
      {fields.map((f) => (
        <ConfigField
          key={f.key}
          field={f}
          value={values[f.key]}
          isSet={!!bridge?.secrets_set.includes(f.key)}
          editing={!!bridge}
          onChange={(v) => set(f.key, v)}
        />
      ))}
      <FormError error={save.error} />
      <button type="submit" disabled={save.isPending}>
        {bridge ? "Save" : "Connect"}
      </button>
    </form>
  );
}

function ConfigField({
  field: f,
  value,
  isSet,
  editing,
  onChange,
}: {
  field: NetworkField;
  value: unknown;
  isSet: boolean;
  editing: boolean;
  onChange: (v: unknown) => void;
}) {
  if (f.kind === "bool") {
    return (
      <label style={{ display: "block", marginBottom: 12 }}>
        <input type="checkbox" checked={Boolean(value)} onChange={(e) => onChange(e.target.checked)} /> {f.label}
        {f.help && <small className="muted"> {f.help}</small>}
      </label>
    );
  }
  const hint = [f.help, f.kind === "secret" && isSet ? "Set; leave empty to keep it" : ""].filter(Boolean).join(". ");
  return (
    <Field label={f.label} hint={hint || undefined}>
      <input
        type={f.kind === "secret" ? "password" : f.kind === "number" ? "number" : "text"}
        value={String(value ?? "")}
        placeholder={f.placeholder}
        required={f.required && !(f.kind === "secret" && (isSet || editing))}
        autoComplete={f.kind === "secret" ? "new-password" : "off"}
        onChange={(e) => onChange(e.target.value)}
      />
    </Field>
  );
}
