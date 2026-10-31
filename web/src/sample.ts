import type { ArtifactFile } from "./main.js";

export const blankFiles: ArtifactFile[] = [
  {
    path: "index.html",
    content: `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Hello</title>
  <style>
    body { margin: 16px; color: #cdd6f4; background: #1e1e2e; font: 14px system-ui, sans-serif; }
    h1 { color: #cba6f7; }
  </style>
</head>
<body>
  <h1>Hello artifact</h1>
</body>
</html>
`,
  },
];

// Several files, to show that relative paths work inside the version folder.
export const sampleFiles: ArtifactFile[] = [
  {
    path: "index.html",
    content: `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Sandbox demo</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <h1>What may this code do?</h1>
  <table id="results"></table>
  <script src="app.js"></script>
</body>
</html>
`,
  },
  {
    path: "style.css",
    content: `body { font: 14px system-ui, sans-serif; margin: 16px; color: #cdd6f4; background: #1e1e2e; }
h1 { font-size: 18px; color: #cba6f7; }
td { padding: 3px 10px 3px 0; vertical-align: top; }
.blocked { color: #a6e3a1; font-weight: 600; }
.allowed { color: #f38ba8; font-weight: 600; }
.info { color: #89b4fa; font-weight: 600; }
`,
  },
  {
    path: "app.js",
    content: `const table = document.getElementById("results");

function row(name, kind, detail) {
  const tr = table.insertRow();
  tr.insertCell().textContent = name;
  const state = tr.insertCell();
  state.textContent = kind === "blocked" ? "blocked" : kind === "allowed" ? "ALLOWED" : "info";
  state.className = kind;
  tr.insertCell().textContent = detail;
}

function attempt(name, action) {
  try {
    row(name, "allowed", String(action()));
  } catch (err) {
    row(name, "blocked", err.name);
  }
}

row("Own URL", "info", location.host);
row("Origin", "info", String(self.origin));

attempt("Read the app DOM", () => parent.document.title);
attempt("Read cookies", () => document.cookie);
attempt("Use localStorage", () => localStorage.length);

fetch("http://app.localhost:8080/api/me", { credentials: "include" })
  .then((res) => row("Call the app API", "allowed", "HTTP " + res.status))
  .catch((err) => row("Call the app API", "blocked", err.name));

fetch("https://example.com/")
  .then((res) => row("Send data out", "allowed", "HTTP " + res.status))
  .catch((err) => row("Send data out", "blocked", err.name));

// The only allowed path to the app: messages the app can inspect and reject.
window.addEventListener("message", (event) => {
  if (event.source === parent && event.data.type === "pong") {
    row("postMessage to the app", "info", "Reply: " + event.data.text);
  }
});
parent.postMessage({ type: "ping" }, "*");
`,
  },
];
