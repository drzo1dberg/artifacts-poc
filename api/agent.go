package main

// The agent loop runs here, in the control plane, and not in the sandbox. The api calls the LLM,
// and only the tool calls run in the microVM. So the VM holds no API key and no session cookie,
// and whatever the model writes there can only reach the outside through the egress policy.
//
//   browser ──► api: agent loop ──► llmgateway (stands in for Foundry) ──► ollama
//                 │ tool calls
//                 ▼
//              sandboxd ──► microVM for this session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	agentMaxSteps       = 16
	agentStepTimeout    = 5 * time.Minute
	agentSessionIdle    = 30 * time.Minute
	agentToolOutputMax  = 8000
	agentCommandTimeout = 120
	agentSiteDir        = "/workspace/site"
	agentMaxSessions    = 2
)

const agentSystemPrompt = `You are a coding agent in a web app that publishes small web pages, called artifacts.
You work in an isolated Linux microVM: Debian 13 with Python 3.13, working folder /workspace, and no internet access.

Your job is to build the web page the user asks for and publish it.

Rules:
- Put every file of the page in /workspace/site. index.html is the entry point. Link other files with relative paths.
- The page runs in a sandboxed iframe with a strict Content Security Policy. It cannot make network requests (fetch, XMLHttpRequest, WebSocket) and cannot load external resources. The only exception is script tags from https://cdnjs.cloudflare.com. Inline scripts and styles work.
- Keep pages self-contained and small. Text files only, no images that need downloading.
- Use run_command to check your work, for example with python3 or ls.
- When the page is ready, call publish. Then answer in one or two sentences what you built.
- Answer in the language the user writes in.`

type agentConfig struct {
	llmURL          string
	llmKey          string
	llmModel        string
	reasoningEffort string
}

type agentSession struct {
	ID        string
	User      string
	SandboxID string

	// run allows one message at a time per session. mu guards the fields below.
	run        sync.Mutex
	mu         sync.Mutex
	messages   []chatMessage
	artifactID string
	version    int
	lastUsed   time.Time
}

type agents struct {
	cfg      agentConfig
	sandbox  *sandboxClient
	http     *http.Client
	mu       sync.Mutex
	sessions map[string]*agentSession
}

func newAgents(cfg agentConfig, sandbox *sandboxClient) *agents {
	return &agents{cfg: cfg, sandbox: sandbox, http: &http.Client{Timeout: agentStepTimeout}, sessions: map[string]*agentSession{}}
}

// --- OpenAI compatible chat completions, the API LiteLLM and Foundry both offer ---

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// modelReply is one assembled answer of the model, with its token counts.
type modelReply struct {
	Content          string
	ReasoningContent string
	ToolCalls        []toolCall
	PromptTokens     int
	CompletionTokens int
}

func tool(name, description string, properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": required},
	}}
}

var agentTools = []map[string]any{
	tool("write_file", "Create or overwrite a text file in the VM. Relative paths start in /workspace.",
		map[string]any{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "path", "content"),
	tool("read_file", "Read a text file in the VM.",
		map[string]any{"path": map[string]string{"type": "string"}}, "path"),
	tool("run_command", "Run a shell command in the VM with /bin/sh in /workspace. Returns exit code, stdout, and stderr.",
		map[string]any{"command": map[string]string{"type": "string"}}, "command"),
	tool("publish", "Publish everything in /workspace/site as a new version of the artifact. index.html is required.",
		map[string]any{"title": map[string]any{"type": "string", "description": "Short title, used only for the first publish"}}),
}

// streamChunk is one server-sent event of a streamed chat completion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// complete asks the model for the next step. It streams the answer so that a long step can report
// progress through onProgress, about once a second, and returns the assembled message.
func (a *agents) complete(ctx context.Context, messages []chatMessage, onProgress func(string)) (*modelReply, error) {
	body := map[string]any{
		"model": a.cfg.llmModel, "messages": messages, "tools": agentTools,
		"stream": true, "stream_options": map[string]bool{"include_usage": true},
	}
	if a.cfg.reasoningEffort != "" {
		body["reasoning_effort"] = a.cfg.reasoningEffort
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.cfg.llmURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.llmKey)
	var (
		content, reasoning strings.Builder
		calls              []toolCall
		chunks             int
		out                modelReply
	)
	start, lastReport, lastChunk := time.Now(), time.Now(), time.Now()
	report := func() {
		phase := "waiting for the first token"
		switch {
		case len(calls) > 0:
			phase = "writing a tool call"
		case chunks > 0 && time.Since(lastChunk) > 2*time.Second:
			// Silence after the first tokens: most likely a tool call that is sent only once
			// it is complete.
			phase = "writing"
		case content.Len() > 0:
			phase = "writing the answer"
		case reasoning.Len() > 0:
			phase = "thinking"
		}
		text := fmt.Sprintf("model is %s · %d s", phase, int(time.Since(start).Seconds()))
		if phase == "thinking" || phase == "writing the answer" {
			text += fmt.Sprintf(" · %d tokens", chunks)
		}
		onProgress(text)
	}

	// Some servers, Ollama among them, send a tool call in one piece once it is complete. Then
	// no chunk arrives while the model writes it, and only this ticker shows that time passes.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// The request runs in its own goroutine, because a gateway can hold back even the response
	// headers until the first token, and that can be the whole tool call.
	lines := make(chan string)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(lines)
		res, err := a.http.Do(req)
		if err != nil {
			readErr <- err
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			readErr <- fmt.Errorf("%s %s", res.Status, truncate(string(raw), 300))
			return
		}
		scanner := bufio.NewScanner(io.LimitReader(res.Body, 16<<20))
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-stop:
				return
			}
		}
		readErr <- scanner.Err()
	}()

	for done := false; !done; {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			report()
			lastReport = time.Now()
		case line, ok := <-lines:
			if !ok {
				done = true
				break
			}
			payload, found := strings.CutPrefix(line, "data:")
			payload = strings.TrimSpace(payload)
			if !found || payload == "" || payload == "[DONE]" {
				continue
			}
			var chunk streamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				return nil, fmt.Errorf("LLM gateway: unexpected chunk %s", truncate(payload, 300))
			}
			if chunk.Usage != nil {
				out.PromptTokens = chunk.Usage.PromptTokens
				out.CompletionTokens = chunk.Usage.CompletionTokens
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" || delta.ReasoningContent != "" || len(delta.ToolCalls) > 0 {
				chunks++
				lastChunk = time.Now()
			}
			content.WriteString(delta.Content)
			reasoning.WriteString(delta.ReasoningContent)
			// Tool calls arrive in parts keyed by index: the first part has the ID and the name,
			// later parts add to the arguments.
			for _, part := range delta.ToolCalls {
				for len(calls) <= part.Index {
					calls = append(calls, toolCall{})
				}
				c := &calls[part.Index]
				if part.ID != "" {
					c.ID = part.ID
				}
				c.Function.Name += part.Function.Name
				c.Function.Arguments += part.Function.Arguments
			}
			if time.Since(lastReport) >= time.Second {
				report()
				lastReport = time.Now()
			}
		}
	}
	if err := <-readErr; err != nil {
		return nil, fmt.Errorf("LLM gateway: %w", err)
	}

	out.Content, out.ReasoningContent = content.String(), reasoning.String()
	for _, c := range calls {
		if c.Function.Name != "" {
			out.ToolCalls = append(out.ToolCalls, c)
		}
	}
	return &out, nil
}

// --- HTTP handlers ---

type agentEvent struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Version    int    `json:"version,omitempty"`
}

func (s *server) createAgentSession(w http.ResponseWriter, r *http.Request, user string) {
	a := s.agents
	a.mu.Lock()
	count := 0
	for _, sess := range a.sessions {
		if sess.User == user {
			count++
		}
	}
	a.mu.Unlock()
	if count >= agentMaxSessions {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d agent sessions per user, end one first", agentMaxSessions))
		return
	}

	id := randomID()
	sandboxID, err := a.sandbox.create(r.Context(), map[string]string{"owner": user, "session": id})
	if err != nil {
		internalError(w, err)
		return
	}
	sess := &agentSession{
		ID: id, User: user, SandboxID: sandboxID, lastUsed: time.Now(),
		messages: []chatMessage{{Role: "system", Content: agentSystemPrompt}},
	}
	a.mu.Lock()
	a.sessions[id] = sess
	a.mu.Unlock()
	log.Printf("agent: session %s for %s in sandbox %s", id, user, sandboxID)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "sandbox_id": sandboxID})
}

func (s *server) agentSession(next func(http.ResponseWriter, *http.Request, *agentSession)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, user string) {
		s.agents.mu.Lock()
		sess := s.agents.sessions[r.PathValue("id")]
		s.agents.mu.Unlock()
		if sess == nil || sess.User != user {
			writeError(w, http.StatusNotFound, "agent session not found")
			return
		}
		next(w, r, sess)
	})
}

func (s *server) deleteAgentSession(w http.ResponseWriter, r *http.Request, sess *agentSession) {
	s.agents.end(r.Context(), sess)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) agentEgress(w http.ResponseWriter, r *http.Request, sess *agentSession) {
	d, err := s.agents.sandbox.decisions(r.Context(), sess.SandboxID)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// sendAgentMessage runs the loop for one user message and streams every step as a line of JSON.
func (s *server) sendAgentMessage(w http.ResponseWriter, r *http.Request, sess *agentSession) {
	var req struct {
		Content string `json:"content"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if !sess.run.TryLock() {
		writeError(w, http.StatusConflict, "the agent is still working on the last message")
		return
	}
	defer sess.run.Unlock()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	emit := func(e agentEvent) {
		enc.Encode(e)
		if flusher != nil {
			flusher.Flush()
		}
	}

	if err := s.runAgent(r.Context(), sess, req.Content, emit); err != nil {
		log.Printf("agent: session %s: %v", sess.ID, err)
		emit(agentEvent{Type: "error", Text: err.Error()})
	}
	emit(agentEvent{Type: "done"})
}

func (s *server) runAgent(ctx context.Context, sess *agentSession, content string, emit func(agentEvent)) error {
	a := s.agents
	sess.mu.Lock()
	sess.messages = append(sess.messages, chatMessage{Role: "user", Content: content})
	sess.lastUsed = time.Now()
	sess.mu.Unlock()

	// unpublished is set by write_file and cleared by publish. Small models sometimes describe the
	// page they built and stop without publishing it, so they get one reminder per message.
	unpublished, reminded := false, false
	for step := 1; step <= agentMaxSteps; step++ {
		sess.mu.Lock()
		history := append([]chatMessage(nil), sess.messages...)
		sess.mu.Unlock()

		emit(agentEvent{Type: "status", Text: fmt.Sprintf("Step %d: waiting for the model", step)})
		start := time.Now()
		msg, err := a.complete(ctx, history, func(text string) {
			emit(agentEvent{Type: "progress", Text: fmt.Sprintf("Step %d: %s", step, text)})
		})
		if err != nil {
			return err
		}
		log.Printf("agent: session %s step %d: %d+%d tokens in %s, %d tool calls", sess.ID, step,
			msg.PromptTokens, msg.CompletionTokens, time.Since(start).Round(time.Millisecond), len(msg.ToolCalls))
		if r := strings.TrimSpace(msg.ReasoningContent); r != "" {
			emit(agentEvent{Type: "reasoning", Text: r})
		}
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = "call_" + randomID()[:8]
			}
			msg.ToolCalls[i].Type = "function"
		}
		sess.mu.Lock()
		sess.messages = append(sess.messages, chatMessage{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
		sess.mu.Unlock()
		if text := strings.TrimSpace(msg.Content); text != "" {
			emit(agentEvent{Type: "assistant", Text: text})
		}
		if len(msg.ToolCalls) == 0 {
			if !unpublished || reminded {
				return nil
			}
			reminded = true
			emit(agentEvent{Type: "status", Text: "The model stopped before publishing, reminding it"})
			sess.mu.Lock()
			sess.messages = append(sess.messages, chatMessage{Role: "user",
				Content: "The files you wrote are not published yet. Call publish now if the page is ready, otherwise say what is missing."})
			sess.mu.Unlock()
			continue
		}

		for _, call := range msg.ToolCalls {
			emit(agentEvent{Type: "tool_call", Name: call.Function.Name, Arguments: call.Function.Arguments})
			output, isError := s.runTool(ctx, sess, call, emit)
			if !isError {
				switch call.Function.Name {
				case "write_file":
					unpublished = true
				case "publish":
					unpublished = false
				}
			}
			emit(agentEvent{Type: "tool_result", Name: call.Function.Name, Text: output, IsError: isError})
			sess.mu.Lock()
			sess.messages = append(sess.messages, chatMessage{Role: "tool", ToolCallID: call.ID, Content: output})
			sess.lastUsed = time.Now()
			sess.mu.Unlock()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("stopped after %d steps without a final answer", agentMaxSteps)
}

// runTool executes one tool call. Errors go back to the model as text, so it can correct itself.
func (s *server) runTool(ctx context.Context, sess *agentSession, call toolCall, emit func(agentEvent)) (string, bool) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Command string `json:"command"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "Error: the arguments are not valid JSON: " + err.Error(), true
	}
	sb := s.agents.sandbox
	sandboxError := func(err error) (string, bool) {
		if errors.Is(err, errSandboxGone) {
			return "Error: the sandbox of this session was deleted after being idle. Ask the user to start a new session.", true
		}
		return "Error: " + err.Error(), true
	}

	switch call.Function.Name {
	case "write_file":
		p := vmPath(args.Path)
		if err := sb.writeFile(ctx, sess.SandboxID, p, args.Content); err != nil {
			return sandboxError(err)
		}
		return fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), p), false

	case "read_file":
		data, err := sb.readFile(ctx, sess.SandboxID, vmPath(args.Path))
		if err != nil {
			return sandboxError(err)
		}
		return truncate(string(data), agentToolOutputMax), false

	case "run_command":
		if strings.TrimSpace(args.Command) == "" {
			return "Error: command is empty", true
		}
		res, err := sb.exec(ctx, sess.SandboxID, args.Command, agentCommandTimeout)
		if err != nil {
			return sandboxError(err)
		}
		out := fmt.Sprintf("exit code %d", res.ExitCode)
		if res.TimedOut {
			out += fmt.Sprintf(" (killed after %d seconds)", agentCommandTimeout)
		}
		if res.Stdout != "" {
			out += "\nstdout:\n" + res.Stdout
		}
		if res.Stderr != "" {
			out += "\nstderr:\n" + res.Stderr
		}
		return truncate(out, agentToolOutputMax), res.ExitCode != 0

	case "publish":
		id, version, err := s.publishFromSandbox(ctx, sess, args.Title)
		if err != nil {
			return sandboxError(err)
		}
		emit(agentEvent{Type: "published", ArtifactID: id, Version: version})
		return fmt.Sprintf("Published version %d. The user sees it in the preview now.", version), false
	}
	return fmt.Sprintf("Error: unknown tool %q", call.Function.Name), true
}

// publishFromSandbox copies /workspace/site out of the VM and publishes it like the editor does.
// The first publish creates the artifact. Later ones build on the version this session published
// last, so a change someone made in the editor in the meantime causes a conflict, not a silent overwrite.
func (s *server) publishFromSandbox(ctx context.Context, sess *agentSession, title string) (string, int, error) {
	sb := s.agents.sandbox
	entries, err := sb.tree(ctx, sess.SandboxID, agentSiteDir)
	if err != nil {
		return "", 0, err
	}
	if len(entries) > maxFiles {
		return "", 0, fmt.Errorf("%s has %d files, at most %d are allowed", agentSiteDir, len(entries), maxFiles)
	}
	var files []file
	var total int64
	for _, e := range entries {
		total += e.Size
		if total > maxBodyBytes {
			return "", 0, fmt.Errorf("the files in %s are larger than %d MiB together", agentSiteDir, maxBodyBytes>>20)
		}
		data, err := sb.readFile(ctx, sess.SandboxID, agentSiteDir+"/"+e.Path)
		if err != nil {
			return "", 0, err
		}
		if !utf8.Valid(data) {
			return "", 0, fmt.Errorf("%s is not a text file, only text files can be published", e.Path)
		}
		files = append(files, file{Path: e.Path, Content: string(data)})
	}
	if msg := validateFiles(files); msg != "" {
		return "", 0, errors.New(msg)
	}

	sess.mu.Lock()
	artifactID, base := sess.artifactID, sess.version
	sess.mu.Unlock()
	var version int
	if artifactID == "" {
		title = strings.TrimSpace(title)
		if title == "" {
			title = "Agent artifact"
		}
		artifactID, err = s.insertArtifact(ctx, sess.User, truncate(title, 100), defaultRetentionDays, files)
		version = 1
	} else {
		version, err = s.publishFiles(ctx, sess.User, artifactID, base, files)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errors.New("the artifact was deleted or expired, it cannot get a new version")
	}
	if err != nil {
		return "", 0, err
	}
	sess.mu.Lock()
	sess.artifactID, sess.version = artifactID, version
	sess.mu.Unlock()
	log.Printf("agent: session %s published %s v%d with %d files", sess.ID, artifactID, version, len(files))
	return artifactID, version, nil
}

func (a *agents) end(ctx context.Context, sess *agentSession) {
	a.mu.Lock()
	delete(a.sessions, sess.ID)
	a.mu.Unlock()
	if err := a.sandbox.delete(ctx, sess.SandboxID); err != nil {
		log.Printf("agent: delete sandbox %s: %v", sess.SandboxID, err)
	}
	log.Printf("agent: session %s ended", sess.ID)
}

// expireIdle ends sessions nobody used for a while. sandboxd deletes idle VMs on its own as well,
// this only keeps the session list in step with it.
func (a *agents) expireIdle(ctx context.Context) {
	a.mu.Lock()
	var idle []*agentSession
	for _, sess := range a.sessions {
		sess.mu.Lock()
		if time.Since(sess.lastUsed) > agentSessionIdle && sess.run.TryLock() {
			sess.run.Unlock()
			idle = append(idle, sess)
		}
		sess.mu.Unlock()
	}
	a.mu.Unlock()
	for _, sess := range idle {
		a.end(ctx, sess)
	}
}

// vmPath turns a path from the model into an absolute path in the VM, relative ones below /workspace.
func vmPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/workspace/" + p
	}
	return path.Clean(p)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
