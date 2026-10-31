import { blankFiles, sampleFiles } from "./sample.js";

export interface ArtifactFile {
  path: string;
  content: string;
}

interface Version {
  number: number;
  created_at: string;
  url: string;
}

interface Artifact {
  id: string;
  title: string;
  live_version: number;
  retention_days: number;
  expires_at: string;
  blob_prefix: string;
  url: string;
  versions?: Version[];
}

interface Me {
  name: string;
  container: string;
}

class ApiError extends Error {
  constructor(public status: number, message: string, public liveVersion?: number) {
    super(message);
  }
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
    throw new ApiError(res.status, data.error, data.live_version);
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
  retentionLabel: el("retention-label"),
  fileSelect: el<HTMLSelectElement>("file-select"),
  addFile: el("add-file"),
  publish: el<HTMLButtonElement>("publish"),
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

function loadDraft(title: string, draft: ArtifactFile[]): void {
  current = null;
  baseVersion = 0;
  files = draft.map((f) => ({ ...f }));
  openFile = "index.html";
  ui.title.value = title;
  ui.title.readOnly = false;
  ui.retention.disabled = false;
  ui.retention.value = "30";
  renderFiles();
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
  current = artifact;
  baseVersion = artifact.live_version;
  files = loaded;
  openFile = "index.html";
  ui.title.value = artifact.title;
  ui.title.readOnly = true;
  ui.retention.disabled = true;
  ui.retention.value = String(artifact.retention_days);
  renderFiles();

  const versions = artifact.versions ?? [];
  ui.versionSelect.replaceChildren(
    ...versions.map((v) => new Option(`Version ${v.number}${v.number === artifact.live_version ? " (live)" : ""}`, String(v.number))),
  );
  renderPreview(versions[0] ?? null);
  setStatus(`Version ${baseVersion} loaded. Changes will be published as version ${baseVersion + 1}.`);
  await refreshList();
}

async function refreshList(): Promise<void> {
  const artifacts = await api<Artifact[]>("GET", "/artifacts");
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
  if (current) {
    try {
      const res = await api<{ id: string; version: number }>("POST", `/artifacts/${current.id}/versions`, {
        base_version: baseVersion,
        files,
      });
      await openArtifact(res.id);
      setStatus(`Version ${res.version} is live.`);
    } catch (err) {
      // Deliberately no automatic retry with the new version number: that would overwrite the other change.
      if (err instanceof ApiError && err.status === 409) {
        setStatus(`Conflict: version ${err.liveVersion} is live now, your work is based on version ${baseVersion}. Reopen the artifact and redo your change there.`, true);
        return;
      }
      throw err;
    }
    return;
  }
  const res = await api<{ id: string; version: number }>("POST", "/artifacts", {
    title: ui.title.value,
    retention_days: Number(ui.retention.value),
    files,
  });
  await openArtifact(res.id);
  setStatus("Version 1 is live.");
}

async function showApp(me: Me): Promise<void> {
  ui.login.hidden = true;
  ui.app.hidden = false;
  ui.whoami.textContent = `${me.name} · container ${me.container}`;
  loadDraft("My artifact", blankFiles);
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
  const version = current?.versions?.find((v) => String(v.number) === ui.versionSelect.value);
  renderPreview(version ?? null);
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

setInterval(() => {
  if (!ui.app.hidden) {
    refreshList().catch(showError);
  }
}, 5000);

api<Me>("GET", "/me").then(showApp).catch(() => {
  ui.login.hidden = false;
});
