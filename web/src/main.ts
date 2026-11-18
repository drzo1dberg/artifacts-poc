import { blankFiles, sampleFiles, type ArtifactFile } from "./sample.js";

interface Version {
  number: number;
  url: string;
}

interface Artifact {
  id: string;
  title: string;
  live_version: number;
  retention_days: number;
  expires_at: string;
  blob_prefix: string;
  versions: Version[];
}

type ArtifactSummary = Pick<Artifact, "id" | "title" | "live_version">;

interface Published {
  id: string;
  version: number;
}

interface Me {
  name: string;
  container: string;
}

interface AgentSession {
  id: string;
  sandbox_id: string;
}

// One line of the NDJSON stream that POST /api/agent/sessions/{id}/messages returns.
interface AgentEvent {
  type: "status" | "reasoning" | "assistant" | "tool_call" | "tool_result" | "published" | "error" | "done";
  text?: string;
  name?: string;
  arguments?: string;
  is_error?: boolean;
  artifact_id?: string;
  version?: number;
}

interface EgressDecision {
  method: string;
  host: string;
  action: string;
}

async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch("/api" + path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) {
    return undefined as T;
  }
  const data = await res.json();
  if (!res.ok) {
    throw new Error(data.error);
  }
  return data as T;
}

function el<T extends HTMLElement>(id: string): T {
  const found = document.getElementById(id);
  if (!found) {
    throw new Error("missing element #" + id);
  }
  return found as T;
}

const ui = {
  login: el<HTMLFormElement>("login"),
  loginName: el<HTMLInputElement>("login-name"),
  app: el("app"),
  whoami: el("whoami"),
  logout: el("logout"),
  newBlank: el("new-blank"),
  newSample: el("new-sample"),
  list: el("list"),
  title: el<HTMLInputElement>("title"),
  retention: el<HTMLInputElement>("retention"),
  fileSelect: el<HTMLSelectElement>("file-select"),
  addFile: el("add-file"),
  publish: el("publish"),
  source: el<HTMLTextAreaElement>("source"),
  status: el("status"),
  versionSelect: el<HTMLSelectElement>("version-select"),
  expire: el<HTMLButtonElement>("expire"),
  delete: el<HTMLButtonElement>("delete"),
  frame: el<HTMLIFrameElement>("frame"),
  blobPath: el("blob-path"),
  contentUrl: el("content-url"),
  expires: el("expires"),
  bridgeLog: el("bridge-log"),
  tabCode: el("tab-code"),
  tabAgent: el("tab-agent"),
  codePane: el("code-pane"),
  agentPane: el("agent-pane"),
  agentSession: el("agent-session"),
  agentEnd: el<HTMLButtonElement>("agent-end"),
  agentLog: el("agent-log"),
  agentForm: el<HTMLFormElement>("agent-form"),
  agentInput: el<HTMLTextAreaElement>("agent-input"),
  agentSend: el<HTMLButtonElement>("agent-send"),
  egressLog: el("egress-log"),
};

// Editor state. current is null while a new artifact has not been published yet.
let current: Artifact | null = null;
let baseVersion = 0;
let files: ArtifactFile[] = [];
let openFile = "";

function setStatus(message: string, isError = false): void {
  ui.status.textContent = message;
  ui.status.classList.toggle("error", isError);
}

function showError(err: unknown): void {
  setStatus(err instanceof Error ? err.message : String(err), true);
}

function saveEditor(): void {
  const file = files.find((f) => f.path === openFile);
  if (file) {
    file.content = ui.source.value;
  }
}

function renderFiles(): void {
  ui.fileSelect.replaceChildren(...files.map((f) => new Option(f.path, f.path)));
  ui.fileSelect.value = openFile;
  ui.source.value = files.find((f) => f.path === openFile)?.content ?? "";
}

function setEditor(artifact: Artifact | null, title: string, retentionDays: string, editorFiles: ArtifactFile[]): void {
  current = artifact;
  baseVersion = artifact?.live_version ?? 0;
  files = editorFiles;
  openFile = "index.html";
  ui.title.value = title;
  ui.title.readOnly = artifact !== null;
  ui.retention.disabled = artifact !== null;
  ui.retention.value = retentionDays;
  renderFiles();
}

function loadDraft(title: string, draft: ArtifactFile[]): void {
  setEditor(null, title, ui.retention.defaultValue, draft.map((f) => ({ ...f })));
  renderPreview(null);
  setStatus("Draft: not published yet.");
  void refreshList();
}

function renderPreview(version: Version | null): void {
  const hasArtifact = current !== null;
  ui.expire.disabled = !hasArtifact;
  ui.delete.disabled = !hasArtifact;
  ui.versionSelect.disabled = !hasArtifact;
  ui.blobPath.textContent = current?.blob_prefix ?? "–";
  ui.expires.textContent = current
    ? new Date(current.expires_at).toLocaleString("en-GB") + " (" + current.retention_days + " days from last publish)"
    : "–";
  ui.contentUrl.textContent = version?.url ?? "–";
  ui.bridgeLog.textContent = "–";
  if (version) {
    ui.frame.src = version.url;
  } else {
    ui.frame.removeAttribute("src");
  }
  if (!current) {
    ui.versionSelect.replaceChildren();
  }
}

async function openArtifact(id: string): Promise<void> {
  const artifact = await api<Artifact>("GET", "/artifacts/" + id);
  const loaded = await api<ArtifactFile[]>("GET", `/artifacts/${id}/versions/${artifact.live_version}/files`);
  setEditor(artifact, artifact.title, String(artifact.retention_days), loaded);

  const versions = artifact.versions;
  ui.versionSelect.replaceChildren(
    ...versions.map((v) => new Option(`Version ${v.number}${v.number === artifact.live_version ? " (live)" : ""}`, String(v.number))),
  );
  renderPreview(versions[0] ?? null);
  setStatus(`Version ${baseVersion} loaded. Changes will be published as version ${baseVersion + 1}.`);
  await refreshList();
}

async function refreshList(): Promise<void> {
  const artifacts = await api<ArtifactSummary[]>("GET", "/artifacts");
  ui.list.replaceChildren(
    ...artifacts.map((a) => {
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = `${a.title} (v${a.live_version})`;
      button.title = a.title;
      button.classList.toggle("active", a.id === current?.id);
      button.addEventListener("click", () => openArtifact(a.id).catch(showError));
      item.append(button);
      return item;
    }),
  );
  // The retention job runs in the background, so the open artifact can disappear at any time.
  if (current && !artifacts.some((a) => a.id === current?.id)) {
    const title = current.title;
    saveEditor();
    loadDraft(title, files);
    setStatus(`"${title}" was deleted or expired. Your text is kept as a new draft.`, true);
  }
}

async function publish(): Promise<void> {
  saveEditor();
  // A conflict surfaces as an error on purpose. Retrying with the new version number would overwrite the other change.
  const res = await (current
    ? api<Published>("POST", `/artifacts/${current.id}/versions`, { base_version: baseVersion, files })
    : api<Published>("POST", "/artifacts", { title: ui.title.value, retention_days: Number(ui.retention.value), files }));
  await openArtifact(res.id);
  setStatus(`Version ${res.version} is live.`);
}

async function showApp(me: Me): Promise<void> {
  ui.login.hidden = true;
  ui.app.hidden = false;
  ui.whoami.textContent = `${me.name} · container ${me.container}`;
  loadDraft("My artifact", blankFiles);
  setInterval(() => {
    if (!document.hidden) {
      refreshList().catch(showError);
    }
  }, 5000);
}

// The bridge between artifact and app. The iframe has origin "null", so we check the source
// through the window object. Nothing goes through here that the artifact may not see.
window.addEventListener("message", (event) => {
  if (event.source !== ui.frame.contentWindow) {
    return;
  }
  ui.bridgeLog.textContent = `received: ${JSON.stringify(event.data)} from origin ${event.origin}`;
  if (event.data?.type === "ping") {
    ui.frame.contentWindow?.postMessage({ type: "pong", text: "Hello from the app" }, "*");
  }
});

ui.login.addEventListener("submit", (event) => {
  event.preventDefault();
  api<Me>("POST", "/login", { name: ui.loginName.value }).then(showApp).catch((err) => alert(err.message));
});

ui.logout.addEventListener("click", () => {
  api("POST", "/logout").then(() => location.reload()).catch(showError);
});

ui.newBlank.addEventListener("click", () => loadDraft("My artifact", blankFiles));
ui.newSample.addEventListener("click", () => loadDraft("Sandbox demo", sampleFiles));

ui.fileSelect.addEventListener("change", () => {
  saveEditor();
  openFile = ui.fileSelect.value;
  renderFiles();
});

ui.addFile.addEventListener("click", () => {
  const path = prompt("File path, for example js/app.js");
  if (!path || files.some((f) => f.path === path)) {
    return;
  }
  saveEditor();
  files.push({ path, content: "" });
  openFile = path;
  renderFiles();
});

ui.publish.addEventListener("click", () => publish().catch(showError));

ui.versionSelect.addEventListener("change", () => {
  if (!current) {
    return;
  }
  // The link of a version expires, so ask for a fresh one instead of reusing the loaded list.
  const number = ui.versionSelect.value;
  api<Artifact>("GET", "/artifacts/" + current.id)
    .then((artifact) => {
      current = artifact;
      renderPreview(artifact.versions.find((v) => String(v.number) === number) ?? null);
    })
    .catch(showError);
});

ui.expire.addEventListener("click", () => {
  if (current) {
    api("POST", `/artifacts/${current.id}/expire-now`)
      .then(() => setStatus("Expired. The retention job cleans up within 10 seconds."))
      .catch(showError);
  }
});

ui.delete.addEventListener("click", () => {
  if (current && confirm(`Delete "${current.title}" for good?`)) {
    api("DELETE", `/artifacts/${current.id}`).then(refreshList).catch(showError);
  }
});

// --- Agent ---
// The api runs the agent loop and calls the model. Tool calls run in a microVM in sandboxd.
// This page only sends messages and shows the steps as they arrive.

let agentSession: AgentSession | null = null;

function showTab(agent: boolean): void {
  ui.codePane.hidden = agent;
  ui.agentPane.hidden = !agent;
  ui.tabCode.setAttribute("aria-selected", String(!agent));
  ui.tabAgent.setAttribute("aria-selected", String(agent));
  if (agent) {
    ui.agentInput.focus();
  }
}

function logLine(className: string, text: string): void {
  const line = document.createElement("p");
  line.className = "msg " + className;
  line.textContent = text;
  ui.agentLog.append(line);
}

function logBlock(text: string, isError = false): void {
  const pre = document.createElement("pre");
  pre.textContent = text;
  pre.classList.toggle("error", isError);
  ui.agentLog.append(pre);
}

function logDetails(summary: string, text: string): void {
  const details = document.createElement("details");
  const title = document.createElement("summary");
  title.textContent = summary;
  const pre = document.createElement("pre");
  pre.textContent = text;
  details.append(title, pre);
  ui.agentLog.append(details);
}

// describeToolCall shows what matters about a call instead of raw JSON with a whole file in it.
function describeToolCall(name: string, rawArgs: string): string {
  try {
    const args = JSON.parse(rawArgs) as Record<string, string>;
    switch (name) {
      case "run_command":
        return "$ " + args.command;
      case "write_file":
        return `write_file ${args.path} (${new Blob([args.content ?? ""]).size} bytes)`;
      case "read_file":
        return "read_file " + args.path;
      case "publish":
        return "publish" + (args.title ? ` "${args.title}"` : "");
    }
  } catch {
    // Fall through and show the raw arguments.
  }
  return `${name} ${rawArgs}`;
}

async function handleAgentEvent(event: AgentEvent): Promise<void> {
  switch (event.type) {
    case "status":
      setStatus(event.text ?? "");
      return;
    case "reasoning":
      logDetails("Thinking", event.text ?? "");
      break;
    case "assistant":
      logLine("assistant", event.text ?? "");
      break;
    case "tool_call":
      logLine("event", "▸ " + describeToolCall(event.name ?? "", event.arguments ?? ""));
      break;
    case "tool_result":
      logBlock(event.text ?? "", event.is_error);
      break;
    case "published":
      logLine("event", `✓ Published version ${event.version}`);
      if (event.artifact_id) {
        await openArtifact(event.artifact_id);
      }
      break;
    case "error":
      logLine("error", event.text ?? "Unknown error");
      break;
    case "done":
      return;
  }
  ui.agentLog.scrollTop = ui.agentLog.scrollHeight;
}

async function startAgentSession(): Promise<AgentSession> {
  setStatus("Starting a microVM for this session...");
  const session = await api<AgentSession>("POST", "/agent/sessions");
  agentSession = session;
  ui.agentSession.textContent = `Session ${session.id.slice(0, 8)} · sandbox ${session.sandbox_id}`;
  ui.agentEnd.disabled = false;
  return session;
}

async function postAgentMessage(session: AgentSession, content: string): Promise<Response> {
  return fetch(`/api/agent/sessions/${session.id}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ content }),
  });
}

async function sendAgentMessage(content: string): Promise<void> {
  let session = agentSession ?? (await startAgentSession());
  logLine("user", content);
  let res = await postAgentMessage(session, content);
  if (res.status === 404) {
    // The api forgot the session, for example after a restart. Start over with a new microVM.
    logLine("event", "The session expired. Starting a new one, files from the old microVM are gone.");
    session = await startAgentSession();
    res = await postAgentMessage(session, content);
  }
  if (!res.ok || !res.body) {
    const data = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(data.error);
  }

  // Each line is one JSON event. A chunk can end in the middle of a line, so keep the rest.
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }
    buffer += value;
    const lines = buffer.split("\n");
    buffer = lines.pop() ?? "";
    for (const line of lines) {
      if (line.trim()) {
        await handleAgentEvent(JSON.parse(line) as AgentEvent);
      }
    }
  }
  setStatus("The agent is done.");
  await showEgress(session);
}

async function showEgress(session: AgentSession): Promise<void> {
  const decisions = await api<EgressDecision[]>("GET", `/agent/sessions/${session.id}/egress`);
  ui.egressLog.textContent = decisions.length
    ? decisions.slice(-3).map((d) => `${d.action} ${d.method} ${d.host}`).join(", ")
    : "no outbound connections";
}

ui.tabCode.addEventListener("click", () => showTab(false));
ui.tabAgent.addEventListener("click", () => showTab(true));

ui.agentForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const content = ui.agentInput.value.trim();
  if (!content) {
    return;
  }
  ui.agentInput.value = "";
  ui.agentSend.disabled = true;
  sendAgentMessage(content)
    .catch((err) => {
      logLine("error", err instanceof Error ? err.message : String(err));
      showError(err);
    })
    .finally(() => {
      ui.agentSend.disabled = false;
    });
});

// Enter sends, Shift+Enter starts a new line.
ui.agentInput.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    ui.agentForm.requestSubmit();
  }
});

ui.agentEnd.addEventListener("click", () => {
  if (!agentSession) {
    return;
  }
  api("DELETE", `/agent/sessions/${agentSession.id}`)
    .then(() => {
      agentSession = null;
      ui.agentLog.replaceChildren();
      ui.agentEnd.disabled = true;
      ui.agentSession.textContent = "No session. Your first message starts a microVM.";
      ui.egressLog.textContent = "–";
      setStatus("Session ended, the microVM is deleted.");
    })
    .catch(showError);
});

api<Me>("GET", "/me").then(showApp).catch(() => {
  ui.login.hidden = false;
});
