import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  type BuiltinAgent,
  type BuiltinAgentSettings,
  createBuiltinAgent,
  createPrincipal,
  type CreatedPrincipal,
  listBuiltinAgents,
  type LLMProvider,
  type MCPServerSettings,
  updateBuiltinAgent,
} from "@say/sdk";
import { type FormEvent, type ReactNode, useState } from "react";
import { keys, Online, useDirectory } from "../../chat";
import { Field, FormError } from "../../components/form";

// People is where instance admins add people (who sign in with a password),
// agents (which get a token) and built-in agents (which the server runs).
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
            <Online principal={p} />
          </li>
        ))}
      </ul>
      <AddPerson />
      <AddAgent />
      <BuiltinAgents />
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

const providers: { value: LLMProvider; label: string; model: string }[] = [
  { value: "anthropic", label: "Anthropic", model: "claude-opus-5" },
  { value: "openai", label: "OpenAI, or compatible (Ollama…)", model: "gpt-5" },
];

const count = new Intl.NumberFormat();

// BuiltinAgents lists the agents the server runs by calling a model, and
// adds and edits them.
function BuiltinAgents() {
  const agents = useQuery({ queryKey: keys.builtinAgents, queryFn: () => listBuiltinAgents() });
  return (
    <section className="builtin-agents">
      <h2>Built-in agents</h2>
      <p className="muted">
        The server runs these by calling a model. Add one to a room like anyone else: it answers direct messages,
        messages that mention it by @name, and every message in rooms where it is set to answer everything.
      </p>
      {agents.error && <FormError error={agents.error} />}
      <ul className="directory">
        {agents.data?.map((a) => <BuiltinAgentRow key={a.principal.id} agent={a} />)}
      </ul>
      <AddBuiltinAgent />
    </section>
  );
}

function useSaveAgent(id: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: BuiltinAgentSettings) => updateBuiltinAgent(id, body),
    onSuccess: (a) =>
      queryClient.setQueryData<BuiltinAgent[]>(keys.builtinAgents, (list) =>
        list?.map((x) => (x.principal.id === a.principal.id ? a : x)),
      ),
  });
}

function BuiltinAgentRow({ agent: a }: { agent: BuiltinAgent }) {
  const [editing, setEditing] = useState(false);
  const save = useSaveAgent(a.principal.id);
  return (
    <li>
      <div>
        {a.principal.display_name} <span className="muted">{a.model}</span>
        {!a.enabled && <span className="badge">off</span>}{" "}
        <span className="muted">
          · {count.format(a.tokens_today)}
          {a.daily_token_budget > 0 && ` of ${count.format(a.daily_token_budget)}`} tokens today
          {a.mcp_servers.length > 0 && ` · tools from ${a.mcp_servers.map((m) => m.name).join(", ")}`}
        </span>{" "}
        <button type="button" className="link" disabled={save.isPending} onClick={() => save.mutate({ enabled: !a.enabled })}>
          {a.enabled ? "Turn off" : "Turn on"}
        </button>{" "}
        <button type="button" className="link" onClick={() => setEditing(!editing)}>
          {editing ? "Close" : "Edit"}
        </button>
      </div>
      {a.last_error && (
        <p role="alert" className="agent-error">
          Last failed to reply: {a.last_error}
        </p>
      )}
      {!editing && <FormError error={save.error} />}
      {editing && (
        <AgentSettingsForm
          agent={a}
          submitLabel="Save"
          pending={save.isPending}
          error={save.error}
          onSubmit={(body) => save.mutate(body, { onSuccess: () => setEditing(false) })}
        />
      )}
    </li>
  );
}

function AddBuiltinAgent() {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [formKey, setFormKey] = useState(0);
  const add = useMutation({
    mutationFn: (body: BuiltinAgentSettings) =>
      createBuiltinAgent({ ...body, display_name: name, provider: body.provider ?? "anthropic" }),
    onSuccess: () => {
      setName("");
      setFormKey((k) => k + 1);
      queryClient.invalidateQueries({ queryKey: keys.builtinAgents });
      queryClient.invalidateQueries({ queryKey: keys.principals });
    },
  });
  return (
    <div>
      <h3>Add a built-in agent</h3>
      <AgentSettingsForm
        key={formKey}
        submitLabel="Add built-in agent"
        pending={add.isPending}
        error={add.error}
        onSubmit={(body) => add.mutate(body)}
      >
        <Field label="Name" hint="People mention it as @name">
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoComplete="off" />
        </Field>
      </AgentSettingsForm>
      {add.isSuccess && <p role="status">Added {add.data.principal.display_name}. Add it to a room to talk to it.</p>}
    </div>
  );
}

// AgentSettingsForm edits a built-in agent's model settings. Only fields
// the admin changed are sent, so a stored API key stays unless replaced.
function AgentSettingsForm({
  agent,
  submitLabel,
  pending,
  error,
  onSubmit,
  children,
}: {
  children?: ReactNode;
  agent?: BuiltinAgent;
  submitLabel: string;
  pending: boolean;
  error: Error | null;
  onSubmit: (body: BuiltinAgentSettings) => void;
}) {
  const [provider, setProvider] = useState<LLMProvider>(agent?.provider ?? "anthropic");
  const [model, setModel] = useState(agent?.model ?? "");
  const [baseUrl, setBaseUrl] = useState(agent?.base_url ?? "");
  const [apiKey, setApiKey] = useState("");
  const [prompt, setPrompt] = useState(agent?.system_prompt ?? "");
  const [maxTokens, setMaxTokens] = useState(String(agent?.max_tokens ?? 1024));
  const [budget, setBudget] = useState(String(agent?.daily_token_budget ?? 0));
  const [servers, setServers] = useState<ServerRow[]>(
    () => agent?.mcp_servers.map((m) => ({ key: nextRow++, name: m.name, url: m.url, auth: "", keep: m.has_authorization })) ?? [],
  );
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const body: BuiltinAgentSettings = {
      provider,
      model,
      base_url: provider === "openai" ? baseUrl : "",
      system_prompt: prompt,
      max_tokens: Number(maxTokens),
      daily_token_budget: Number(budget),
      mcp_servers: servers.map((r): MCPServerSettings => {
        const m: MCPServerSettings = { name: r.name, url: r.url };
        // Left out, the server keeps a stored header for the same name and URL.
        if (r.auth || !r.keep) m.authorization = r.auth;
        return m;
      }),
    };
    if (apiKey) body.api_key = apiKey;
    onSubmit(body);
  };
  const env = provider === "anthropic" ? "ANTHROPIC_API_KEY" : "OPENAI_API_KEY";
  return (
    <form onSubmit={submit} className="agent-settings">
      {children}
      <Field label="Provider">
        <select value={provider} onChange={(e) => setProvider(e.target.value as LLMProvider)}>
          {providers.map((p) => (
            <option key={p.value} value={p.value}>
              {p.label}
            </option>
          ))}
        </select>
      </Field>
      <Field label="Model" hint="Empty picks the provider's default">
        <input
          value={model}
          onChange={(e) => setModel(e.target.value)}
          placeholder={providers.find((p) => p.value === provider)?.model}
          autoComplete="off"
        />
      </Field>
      {provider === "openai" && (
        <Field label="Base URL" hint="For an OpenAI-compatible server, e.g. http://localhost:11434/v1 for Ollama">
          <input type="url" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} autoComplete="off" />
        </Field>
      )}
      <Field
        label="API key"
        hint={
          agent?.has_api_key
            ? "A key is stored; leave empty to keep it"
            : `Leave empty to use the server's ${env}`
        }
      >
        <input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} autoComplete="new-password" />
      </Field>
      <Field label="Instructions" hint="Who the agent is and how it should behave">
        <textarea rows={4} value={prompt} onChange={(e) => setPrompt(e.target.value)} maxLength={20000} />
      </Field>
      <Field label="Longest reply, in tokens">
        <input type="number" min={1} max={64000} required value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} />
      </Field>
      <Field label="Tokens per day" hint="Input and output together, per UTC day; 0 is no limit">
        <input type="number" min={0} required value={budget} onChange={(e) => setBudget(e.target.value)} />
      </Field>
      <MCPServers rows={servers} onChange={setServers} />
      <FormError error={error} />
      <button type="submit" disabled={pending}>
        {submitLabel}
      </button>
    </form>
  );
}

type ServerRow = { key: number; name: string; url: string; auth: string; keep: boolean };
let nextRow = 0;

// MCPServers edits the MCP servers whose tools an agent may call. A stored
// Authorization header is never shown; it is kept unless replaced or
// forgotten.
function MCPServers({ rows, onChange }: { rows: ServerRow[]; onChange: (rows: ServerRow[]) => void }) {
  const set = (key: number, change: Partial<ServerRow>) =>
    onChange(rows.map((r) => (r.key === key ? { ...r, ...change } : r)));
  return (
    <fieldset className="mcp-servers">
      <legend>Tools</legend>
      <p className="muted">
        MCP servers (streamable HTTP) whose tools the agent may call. It sees them as <code>name__tool</code>.
      </p>
      {rows.map((r) => (
        <div key={r.key} className="mcp-server">
          <input
            aria-label="Server name"
            placeholder="name"
            value={r.name}
            onChange={(e) => set(r.key, { name: e.target.value })}
            required
            pattern="[a-zA-Z0-9_\-]{1,32}"
            title="1 to 32 letters, digits, _ or -"
            autoComplete="off"
          />
          <input
            aria-label="Server URL"
            type="url"
            placeholder="https://example.com/mcp"
            value={r.url}
            onChange={(e) => set(r.key, { url: e.target.value })}
            required
            autoComplete="off"
          />
          <input
            aria-label="Authorization header"
            type="password"
            placeholder={r.keep ? "Header stored" : "Authorization"}
            title={r.keep ? "A header is stored; leave empty to keep it" : "Sent as the Authorization header, e.g. Bearer …"}
            value={r.auth}
            onChange={(e) => set(r.key, { auth: e.target.value })}
            autoComplete="new-password"
          />
          <span className="mcp-actions">
            {r.keep && (
              <button type="button" className="link" onClick={() => set(r.key, { keep: false })}>
                Forget header
              </button>
            )}
            <button type="button" className="link" onClick={() => onChange(rows.filter((x) => x.key !== r.key))}>
              Remove
            </button>
          </span>
        </div>
      ))}
      {rows.length < 8 && (
        <button
          type="button"
          className="link"
          onClick={() => onChange([...rows, { key: nextRow++, name: "", url: "", auth: "", keep: false }])}
        >
          Add MCP server
        </button>
      )}
    </fieldset>
  );
}
