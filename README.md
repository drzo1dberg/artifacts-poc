# artifacts-poc

My learning project: how would you build something like Claude Artifacts, where a user publishes a web page and the platform runs it safely? And how does an agent fit in that writes such a page by running code on the server?

I wanted to understand the systems design, so I rebuilt the idea locally with Docker and no cloud services. I built it together with Claude Code. This is a POC with a fake login, not something to deploy.

![The app with the sandbox demo open](docs/screenshot.png)

![The agent tab: the model wrote, checked, and published a clock page from inside a microVM](docs/agent.png)

## Run it

You need a Linux host with Docker Compose 2.24 or later and with virtualization turned on, so that `/dev/kvm` exists. `setup.sh` checks the host, downloads the model, and starts the stack:

```bash
git clone https://github.com/drzo1dberg/artifacts-poc.git
cd artifacts-poc
./setup.sh
```

Then open <http://app.localhost:8080> and log in with any name. Chrome and Firefox resolve `*.localhost` on their own, Safari does not. The first run downloads several GB of Docker images and the 9.5 GB model and takes a few minutes. You can run `setup.sh` again at any time; it skips the steps that are done.

### The model

By default, `setup.sh` downloads `Qwen3.5-9B-Q8_0.gguf` from [lmstudio-community/Qwen3.5-9B-GGUF](https://huggingface.co/lmstudio-community/Qwen3.5-9B-GGUF) on Hugging Face. Qwen publishes no GGUF of this model itself; LM Studio converts the official weights from [Qwen/Qwen3.5-9B](https://huggingface.co/Qwen/Qwen3.5-9B) with llama.cpp. The script pins the file to a commit and checks its SHA-256.

The file goes into `models/`. The `models` job in `docker-compose.yml` runs `ollama/create-model.sh`, which builds the model `agent-llm` from the first GGUF file it finds there. A GGUF file holds the weights and a chat template, but Ollama returns tool calls and thinking as separate API fields only with its own renderer and parser, so the script sets `RENDERER qwen3.5` and `PARSER qwen3.5`.

To get the model from the Ollama registry instead, run `./setup.sh --model ollama`. Without a GGUF file in `models/`, a plain `docker compose up -d --build` does the same.

### The GPU

`setup.sh` picks the GPU on its own. To choose yourself, pass `--gpu`:

| Option | Hardware | How Ollama uses it |
|---|---|---|
| `vulkan` | AMD or Intel | Vulkan through `/dev/dri`, the default in `docker-compose.yml`. I use an AMD Radeon RX 9060 XT with 16 GB VRAM. |
| `nvidia` | NVIDIA, with the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html) | CUDA. Untested, because I don't have an NVIDIA card. |
| `cpu` | none | The CPU. A page then takes several minutes. |

On the RX 9060 XT, Vulkan generated about 30 % more tokens per second than ROCm in my own comparison, and it needs no ROCm installation.

For `nvidia` and `cpu`, the script writes `docker-compose.override.yml`, which Docker Compose reads together with `docker-compose.yml`. With the 32k context, the model takes about 10 GB of VRAM.

To stop the stack, run `docker compose down`. To also delete all data, including the model, run `docker compose down -v`.

## The one idea that explains the design

An artifact is untrusted code that runs next to your logged-in session. Without protection, it could call the API as you. Almost every decision here follows from that:

1. **App and content live on separate origins**, so the code never sees the session cookie.
2. **The browser enforces the limits** through sandboxing and a Content Security Policy.
3. **Versions never change.** A publish writes a new folder and then moves a pointer in the database, which makes every version safe to cache.
4. **Control plane and data plane are separate.** The API decides and keeps records. The front door serves content from blob storage.

## Architecture

```
                         Browser
        ┌───────────────────┴────────────────────┐
        │ app.localhost:8080                     │ <id>--<user>.usercontent.localhost:8080
        │ app and session cookie                 │ artifact in a sandboxed iframe
        └───────────────────┬────────────────────┘
                      ┌─────┴─────┐
                      │ frontdoor │  nginx: routing by host, edge cache, CSP
                      └─┬───┬───┬─┘
               /        │   │   │  usercontent: GET and HEAD only
         ┌──────────────┘   │   └─────────────────┐
      ┌──┴──┐  /api/   ┌────┴───┐    writes    ┌───┴───────┐
      │ web │          │  api   │ ───────────► │ blobstore │
      └─────┘          └─┬──┬─┬─┘              └───────────┘
                         │  │ └──────────────────────┐
              ┌──────────┘  └──────┐                 │ tool calls
         ┌────┴─────┐        ┌─────┴──────┐    ┌─────┴─────┐
         │ postgres │        │ llmgateway │    │ sandboxd  │  one Firecracker microVM per sandbox
         └──────────┘        └─────┬──────┘    └───────────┘
                               ┌───┴────┐
                               │ ollama │  Qwen3.5 9B on the GPU
                               └────────┘
```

| Service | Built with | Job | Code |
|---|---|---|---|
| `frontdoor` | nginx | The only way in | `frontdoor/default.conf` |
| `blobstore` | Go, standard library | Simulated storage account | `blobstore/main.go` |
| `api` | Go, pgx | Control plane and retention job | `api/main.go`, `api/blob.go`, `api/token.go` |
| `postgres` | PostgreSQL 17 | Metadata | `api/schema.sql` |
| `sandboxd` | Go, Firecracker | Simulated ACA Sandboxes: microVMs for untrusted agent code | `sandboxd/` |
| `llmgateway` | LiteLLM | Simulated Azure AI Foundry: an OpenAI compatible endpoint with a key | `llmgateway/config.yaml` |
| `ollama` | Ollama with Vulkan | Runs the model on the GPU | `docker-compose.yml` |
| `models` | Ollama CLI | One-shot job: downloads the model and sets a 32k context | `docker-compose.yml` |
| `web` | TypeScript, no framework | Editor and preview | `web/src/main.ts` |

Each user gets one container, each artifact is a folder, and each version is a subfolder:

```
stlocal/                     storage account
└── u-alice/                 container, one per user
    └── <artifact-id>/       artifact
        ├── v1/              version, never changes
        │   ├── index.html
        │   └── js/app.js
        └── v2/
            └── index.html
```

## How it works

### Publishing

`publishVersion` in `api/main.go` does this:

```
1. BEGIN; SELECT live_version ... FOR UPDATE   parallel publishes wait here
2. base_version differs from live?             return 409, nothing changes
3. upload blobs to <id>/v(n+1)/                invisible, nothing points to them
4. record the version, set live_version = n+1
5. COMMIT                                      only now is it live
```

The commit is the switch. If the service dies before it, the worst case is a few orphaned blobs, and the next attempt clears them.

There are two kinds of locking here and I had to think about why. `FOR UPDATE` protects the few milliseconds of the check. `base_version` protects the minutes between loading an artifact and publishing it. If someone else published in between, you get a conflict instead of silently overwriting their work.

### Reading

```
GET <id>--alice.usercontent.localhost:8080/t/<token>/v2/index.html
  -> the hostname regex validates the ID and the user
  -> the API checks the token and that the artifact still exists
  -> edge cache: a HIT ends here
  -> blobstore: /u-alice/<id>/v2/index.html
```

You only get a link after logging in. The login is just a username, no password. The API signs a token for the owner, valid for 15 minutes and for one version of one artifact, and puts it into the path. It has to be the path, because relative URLs inherit it, so `style.css` and `app.js` carry the token automatically.

The link also refuses to open directly. Browsers send `Sec-Fetch-Dest: document` for a top-level visit and `iframe` inside our app, and scripts on the page cannot change that. So the content is only viewable on our canvas. curl can fake the header, so the token is what actually protects the content.

The front door asks the API before every read, using nginx `auth_request`, and this runs before the cache. That makes a delete effective immediately. The price is that reads now depend on the API and the database, so artifacts stop loading when the API is down. Only Postgres knows which version is live. The app asks the API and puts that link into the iframe.

Because `v2` never changes, publishing needs no cache invalidation. The next version simply has a new URL. The token stays out of the cache key, so new links still hit the cache.

### Isolation

Four independent layers:

| Layer | Where | What it stops |
|---|---|---|
| Separate domain | front door | The cookie is host-only and never reaches `usercontent` |
| `sandbox="allow-scripts"` | `web/index.html` | The origin becomes `null`: no DOM, cookies or storage |
| CSP header with `sandbox` | `frontdoor/default.conf` | Applies even when someone opens the URL without the iframe |
| `connect-src 'none'` | same header | `fetch` cannot call the API or send data out |

The only channel left is `postMessage`. The app checks the sending window, because the origin `null` tells it nothing. A feature like "ask Claude from an artifact" would use this bridge: the artifact asks, the app decides.

### Retention

The default is 30 days per artifact, and every publish restarts the clock. `purgeExpired` in `api/main.go` runs every 10 seconds, deletes the blobs and then writes a tombstone row.

- **Blobs first, database second.** After a crash in between, the job just runs again, because deleting is idempotent. The other order could leave data behind that nobody can find.
- **`FOR UPDATE SKIP LOCKED`** skips artifacts that a publish is working on.
- **Deleted means gone.** The API refuses the read even if the file is still in the edge cache, and the cached copy expires within 60 seconds.

## The agent

The **Agent** tab takes a request like "a click counter with a table of squares". The model writes the files, runs `python3` to generate data or check its work, and publishes the result as a new version of an artifact.

### Where the loop runs

There are two common designs for an agent with a sandbox:

1. **The whole agent runs in the sandbox.** The harness, its tools, and its calls to the model all run in the VM, and an egress proxy keeps credentials outside. Claude Code on the web runs each session in an isolated VM like this.
2. **The loop runs in the backend, and only tool calls run in the sandbox.** The backend calls the model, and when the model asks for a tool, the backend runs it in the VM. The exec and file operations of ACA Sandboxes are made for this.

This project uses the second design, because the app itself should make the call to the model. `runAgent` in `api/agent.go` does this:

```
1. send the history and four tools to llmgateway   POST /v1/chat/completions
2. the model answers with tool calls               write_file, read_file, run_command, publish
3. run each one in this session's microVM          sandboxd exec and files
4. append the results, go to 1                     at most 16 steps
5. no more tool calls                              the answer is final
```

Every step goes to the browser as one line of JSON while the loop runs. The front door turns off response buffering for `/api/agent/`, otherwise the lines would arrive all at once at the end.

A single step can take more than a minute when the model writes a whole page. So the api requests the answer as a stream and sends a progress line every second, for example `Step 1: model is thinking · 4 s · 120 tokens`. Ollama streams thinking and text token by token, but sends a tool call in one piece once it is complete. While the model writes a tool call, the progress line can therefore only count seconds.

The VM holds no API key and no session cookie. Whatever the model writes there can only reach the outside through the egress policy, and the agent sessions get none: an attempt to `pip install` something fails, shows up in the decision log, and the model reads the error like any other tool result.

### Publishing from the sandbox

`publish` copies `/workspace/site` out of the VM and runs the same code as the editor: `insertArtifact` for the first version, then `publishFiles` with `base_version` set to the version this session published last. If someone changes the artifact in the editor in the meantime, the agent gets the conflict as a tool result instead of silently overwriting that change.

### Sessions

Each session gets its own microVM. After 60 seconds without a tool call, `sandboxd` suspends it with a memory snapshot, and the next message resumes it in about 25 ms with all files intact. After 30 idle minutes, both the session and the VM are deleted. A user can have at most two sessions.

### The model

The model is Qwen3.5 9B, quantized to 8 bits (Q8_0). I chose it with a benchmark of the actual use case: the agent built each of four pages three times, through the api, the microVM, and the publish path. A script then loaded every published page in Chromium and used it, for example by clicking 7 × 6 = on the calculator and expecting 42.

| Model, all from GGUF files | In memory | Tokens per second | Pages that work | Median time per page |
|---|---|---|---|---|
| Qwen3.5 9B Q4_K_M, 5.6 GB | 6.4 GB, all on the GPU | 41 | 9 of 12 | 41 s |
| Qwen3.5 9B Q6_K, 7.4 GB | 7.9 GB, all on the GPU | 35 | 10 of 12 | 47 s |
| **Qwen3.5 9B Q8_0, 9.5 GB** | 9.7 GB, all on the GPU | 30 | **12 of 12** | 59 s |
| Qwen3.6 35B-A3B Q4_K_M, 20.5 GB | 21 GB, 30 % in system RAM | 27 | 11 of 12 | 88 s |

Measured on an AMD Radeon RX 9060 XT (16 GB, Vulkan) with a Ryzen 7 3700X and DDR4. Tokens per second include reading the prompt. Twelve runs per model are a small sample, so read the differences as a trend, not as proof.

What the numbers show:

- **Speed follows file size.** To generate one token, the GPU reads every active weight once, so the memory bandwidth sets the limit. Q8_0 is 1.7 times the size of Q4_K_M and about 1.4 times slower.
- **Fewer bits cause typos in code.** The failures of the smaller quantizations were real bugs: `newDate` instead of `new Date`, a calculator that showed `NaN`, and a table with 29 instead of 30 primes. A 9B model has few weights to absorb rounding errors, and one wrong token breaks a page.
- **The MoE model is smarter but slower here.** Qwen3.6 35B-A3B has 35 billion weights, but only about 3 billion take part in each token. That keeps it usable even with 30 % of the model in system RAM. It was the only model that used `python3` for the primes task every time, as the prompt asked; the 9B models did so once in three runs. On this card it is still slower, and it needs a 20.5 GB download. For longer tasks with more steps, it is the better choice.
- **A dense 27B model does not fit.** At 16.5 GB, part of it would run from system RAM for every token, at a fraction of the GPU's bandwidth.

The api only knows an endpoint, a key, and the model name `agent`. LiteLLM maps that name to Ollama, so moving to Azure AI Foundry is a change in `llmgateway/config.yaml`, not in the code. The file has a commented example.

Thinking takes most of the time in short steps. With `AGENT_REASONING_EFFORT: none` in `docker-compose.yml`, a short first step took about 3 s instead of 11 s in a measurement with Q4_K_M. Writing a whole page in one tool call takes 30 to 80 s either way.

A 9B model is a weak agent compared to a frontier model. Small pages work reliably. Long tasks with many steps go wrong more often. Sometimes the model describes the page it wrote and stops without calling `publish`. In that case `runAgent` reminds it once, and the model then publishes or explains why it doesn't.

## Sandboxes for agent code

Artifacts run in the browser. An agent also needs a place to run code on the server: it writes files, runs `python3`, and installs packages. That code is just as untrusted as an artifact, and a container is not enough of a boundary for it, because containers share the host kernel. Cloud agent products therefore run tool calls in a microVM with its own kernel. On Azure, that's [Azure Container Apps Sandboxes](https://sandboxes.azure.com/docs/sandboxes/), which reached general availability in September 2026.

`sandboxd` rebuilds that model locally with [Firecracker](https://github.com/firecracker-microvm/firecracker), the VMM behind AWS Lambda:

```
api ──HTTP──► sandboxd ──Unix socket──► firecracker ──vsock──► sandbox-agent (PID 1 in the VM)
                  ▲                                                 │
                  └──── egress proxy ◄── vsock ◄── 127.0.0.1:3128 ◄─┘ HTTP_PROXY of every command
```

- **Each sandbox is a VM with its own kernel.** A disk image is a normal container image, here `python:3.13-slim`, with a small agent as `/sbin/sandbox-agent`. On startup, `sandboxd` turns it into an ext4 file with `mkfs.ext4 -d`, which needs neither root nor loop devices. Each sandbox boots from its own copy. On btrfs, that copy is a reflink and takes no time.
- **The VM has no network card.** The host reaches the agent over vsock. Firecracker maps vsock to a Unix socket, so `sandboxd` needs no network rights at all, only `/dev/kvm`.
- **All outbound traffic goes through the egress proxy.** Commands get `HTTP_PROXY` and `HTTPS_PROXY`, the agent forwards those connections over vsock, and `sandboxd` decides. The first matching rule wins, otherwise the default action applies. Private, loopback, and link-local addresses are always blocked, after DNS resolution, so even `default_action: Allow` can't reach `api`, `postgres`, or a metadata endpoint.
- **Secrets stay outside.** A rule with action `Transform` sets headers on the way out. A value like `Bearer {secret:DEMO}` is filled in from the `SANDBOX_SECRET_DEMO` environment variable of `sandboxd`. The VM never sees it. As in ACA with partial traffic inspection, HTTPS is only checked by host name, so header changes only work for plain HTTP.
- **Stop keeps the memory.** Stop pauses the VM, writes a full Firecracker snapshot of memory and devices, and ends the process. Resume loads it in a new process. Files in `/dev/shm` and running background processes survive this. A request to a stopped sandbox resumes it first.
- **Idle sandboxes clean themselves up.** `auto_suspend_seconds` stops a sandbox after that much idle time and `auto_delete_seconds` deletes it.

Measured on a Ryzen 7 3700X with btrfs:

| Operation | Time |
|---|---|
| Create, from copy to agent ready | 640 ms |
| Stop with a memory snapshot of 512 MiB | 800 ms |
| Resume from the snapshot | 25 ms |
| A command with `uname` and `python3` through exec | 65 ms |

The API follows the operations of ACA Sandboxes. Microsoft documents those as CLI and SDK calls, not as REST paths, so the paths here are my own:

| `sandboxd` | ACA Sandboxes CLI |
|---|---|
| `POST /sandboxes` with `disk`, `labels`, `lifecycle`, `egress` | `aca sandbox create --disk python-3.13 --label ...` |
| `GET /sandboxes?label=key=value` | `aca sandbox list -l key=value` |
| `POST /sandboxes/{id}/exec` | `aca sandbox exec --id ... -c "..."` |
| `GET`, `PUT /sandboxes/{id}/files?path=...`, `GET /sandboxes/{id}/tree?path=...` | `aca sandbox fs cat`, `fs write`, `fs ls` |
| `POST /sandboxes/{id}/stop`, `/resume` | `aca sandbox stop`, `resume` |
| `PUT /sandboxes/{id}/lifecycle` | `aca sandbox lifecycle set` |
| `PUT /sandboxes/{id}/egress`, `GET .../egress/decisions` | `aca sandbox egress set`, `egress decisions` |
| `DELETE /sandboxes/{id}` | `aca sandbox delete` |

One deliberate difference: in ACA, a sandbox without an egress policy has unrestricted outbound access. Here it has none.

## Try it

**Sandbox:** log in, click "Load sandbox demo", then "Publish". The artifact tries to break out and reports the results in a table.

**Conflict:** open the same artifact in two tabs and publish from both. The second tab gets a 409.

**Retention:** click "Expire now" and watch the job clean up within 10 seconds:

```bash
docker compose logs -f api                                  # retention: removed ...
docker compose exec blobstore find /data -type f            # the folder is gone
docker compose exec postgres psql -U artifacts -c \
  "select title, deleted_at, delete_reason from artifacts"  # the tombstone stays
```

**Login gate:** copy the front door URL shown in the app and open it in a new tab. You get a 403, because only the iframe in the app may show it. Remove the token from the URL and you get a 404.

**Agent:** log in, open the **Agent** tab, and ask for a page. Then ask for a change: the agent publishes it as the next version. Ask it to install a package with `pip`, and the **Sandbox egress** line shows the denied connection.

**MicroVM:** create a sandbox, run code in it, and watch the egress policy work:

```bash
alias sb='docker compose exec sandboxd curl -sS -H "Authorization: Bearer local-development-only"'
sb -d '{"disk":"python"}' localhost:8090/sandboxes                     # note the id
sb -d '{"command":"uname -r; python3 -V"}' localhost:8090/sandboxes/ID/exec
sb -d '{"command":"pip download -q requests -d /tmp"}' localhost:8090/sandboxes/ID/exec  # 403, no egress by default
sb localhost:8090/sandboxes/ID/egress/decisions
sb -X POST localhost:8090/sandboxes/ID/stop                              # memory snapshot
sb -X DELETE localhost:8090/sandboxes/ID
```

**Edge cache:** run `curl -I -H "Sec-Fetch-Dest: iframe"` twice on that URL. You get `X-Cache-Status: MISS`, then `HIT`.

## How this would look on Azure

I have not deployed this. The mapping comes from reading the Microsoft Learn docs in October 2026. Treat it as a design sketch.

| POC | Azure |
|---|---|
| nginx | Front Door Standard with managed identity origin auth |
| blobstore | Storage account with Shared Key and anonymous access turned off |
| container per user | Blob container, the docs list no limit per account |
| Go API | Container Apps, with a managed identity for storage access |
| postgres | Azure Database for PostgreSQL Flexible Server |
| fake login | Entra ID, assuming all users are internal, and a signed session cookie |
| retention job | Container Apps job, plus a lifecycle policy as a safety net |
| sandboxd | Container Apps Sandboxes: a sandbox group, the `python-3.13` disk image, and an egress policy per sandbox |
| llmgateway and ollama | Azure AI Foundry with a model deployment, called by the API with a managed identity |

The interesting part is what does not map one to one:

**Hostname to path.** Front Door rule sets have no capture groups I could use, so nginx's regex has no direct equivalent. They do offer substrings of the `hostname` variable. This only works here because a UUID has a fixed length, so for `*.usercontent.example.com` a URL rewrite with preserve unmatched path could look like this. I have not tested it:

```
/u-{hostname:38:-24}/{hostname:0:36}/
```

**Wildcard domain.** `*.usercontent.example.com` needs a wildcard certificate, which I would keep in Key Vault.

**Caching needs explicit headers.** Without a `Cache-Control` header, Front Door picks a random cache time of one to three days. It also refuses to cache `no-cache`, which the POC sends to browsers. So the edge cache time needs an explicit rule or a header like `s-maxage=60, max-age=0`.

**Purging is weak, and the design does not need it for publishing.** Purge can take up to 10 minutes and is not directly supported for wildcard domains, so you have to name each subdomain. Microsoft itself recommends versioned URLs for updates, which is what this design does. A real delete is different: a guaranteed quick removal stays an open problem, and a short cache time is the simple answer.

**"Retention policy" means two things.** A lifecycle management policy deletes data after a while. An immutability policy forbids deleting it for a while. This project wants the first one.

**A lifecycle policy alone is not enough.**

- It knows nothing about the database, so the tombstone and the "restart on publish" logic stay in the job.
- It works on blob age, so it could remove an old version of an artifact that is still in use.
- It allows 100 rules per policy, so one rule per user does not scale. One rule per retention class does.
- Changes can take up to 24 hours and runs have no fixed schedule, so a per-artifact deadline needs the job anyway.

**Folders are a real decision.** Without hierarchical namespace there are no folders, so deleting an artifact means listing its blobs and deleting them in a batch. With hierarchical namespace you get real folders, but lifecycle rules can no longer filter on blob index tags, because that filter is for flat namespace accounts only.

**Entra users do not fit into the hostname as they are.** With internal users, the container name could come from the Entra object ID, but that is a 36 character UUID. Two UUIDs plus `--` make 74 characters, and a DNS label allows 63. The API would have to keep a short slug per user in Postgres.

### No SAS tokens

SAS tokens are not always bad. Key-signed SAS tokens are the ones to avoid, because they depend on the account key. Microsoft recommends a user delegation SAS when someone without an identity needs temporary access. This design has no such caller, because every caller can have a managed identity:

| Caller | Identity | Role |
|---|---|---|
| API | User-assigned managed identity | Storage Blob Data Contributor |
| Front Door | User-assigned managed identity | Storage Blob Data Reader |

Turning off Shared Key on the account means key-signed SAS tokens stop working. Front Door cannot write, because its role does not allow it. The API creates the per-user containers at login with its identity.

The tradeoff is that managed identity origin auth does not work with Private Link. The storage account stays reachable over the internet and relies on Entra ID alone. That is why this would use Standard and not Premium.

**Private artifacts need more than Front Door.** The POC gates reads with a signed link that the front door checks through the API. Entra login alone does not do that on Azure, because Front Door reads with its own identity and not the user's. A SAS token would not help either, because it does not carry over to the relative `style.css` and `app.js`.

## Where this differs from real Claude Artifacts

Documented by the artifact server Claude uses: a sandboxed frame, a CSP that blocks `fetch` and allows a few CDNs, private by default, a UUID as slug, `index.html` as the entry point, relative paths across several files, and versions with a conflict check on `base_version`.

Everything behind that is my own model. How Anthropic stores, routes and retains artifacts is not public. Containers per user, the hostname scheme and retention are what I wanted to learn, not a copy of their internals. This project is not affiliated with Anthropic.

## What I learned

- Security here comes down to one question: how do you show code without giving it your session? Answering it shaped the whole architecture.
- Immutable versions make hard problems disappear: publishing needs no cache invalidation.
- The order of two steps decides what a crash leaves behind, both when publishing and when deleting.
- Mapping to the cloud teaches more than the happy path. Most of what I learned about Azure came from the things that do not translate one to one.

## Known shortcuts

- The login is only a username and the cookie is not signed, so anyone can be anyone.
- A copied link stays valid for up to 15 minutes, even after logging out, for anyone who fakes the fetch header.
- A page left open longer than 15 minutes cannot load further files until the artifact is reopened.
- There is no HTTPS.
- Every publish sends all files, and only text files work, because they travel as JSON strings.
- A publish holds a database lock during the upload. It is correct but does not scale. A real version would upload under a random ID first and then move the pointer with `UPDATE ... WHERE live_version = base`.
- The database password in `docker-compose.yml` is a local development value. Postgres has no published port.
- The api keeps agent sessions in memory. A restart loses them, and the next message starts a new microVM.
- One GPU serves every user, and Ollama answers one request at a time.
- `sandboxd` keeps its sandboxes in memory. A restart of the service deletes all of them.
- Firecracker runs without its `jailer`, so the VMM processes run as root in the `sandboxd` container instead of in a chroot with their own user and cgroup.
- The egress proxy only knows HTTP and HTTPS through `HTTP_PROXY`. A program that ignores the proxy variables has no network.
