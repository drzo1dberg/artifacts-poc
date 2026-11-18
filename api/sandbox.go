package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sandboxClient talks to sandboxd, the local stand-in for ACA Sandboxes. On Azure, the same calls
// would go to the data plane of a sandbox group, authenticated with a managed identity.
type sandboxClient struct {
	base  string
	token string
	http  *http.Client
}

var errSandboxGone = errors.New("sandbox not found")

type execResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}

type treeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type egressDecision struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Action string    `json:"action"`
	Reason string    `json:"reason"`
}

func newSandboxClient(base, token string) *sandboxClient {
	return &sandboxClient{base: base, token: token, http: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *sandboxClient) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 12<<20))
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusNotFound && strings.Contains(string(data), "sandbox not found") {
		return errSandboxGone
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("sandboxd %s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("sandboxd %s %s: %s %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	if b, ok := out.(*[]byte); ok {
		*b = data
		return nil
	}
	return json.Unmarshal(data, out)
}

func jsonBody(v any) io.Reader {
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

// create starts a microVM for one agent session. It suspends after a minute without tool calls and
// is deleted after 30 idle minutes. It gets no egress at all: an agent that builds a web page does
// not need the internet, and every attempt shows up in the decision log.
func (c *sandboxClient) create(ctx context.Context, labels map[string]string) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "POST", "/sandboxes", jsonBody(map[string]any{
		"disk":      "python",
		"labels":    labels,
		"lifecycle": map[string]int{"auto_suspend_seconds": 60, "auto_delete_seconds": 1800},
		"egress":    map[string]any{"default_action": "Deny", "rules": []any{}},
	}), &res)
	return res.ID, err
}

func (c *sandboxClient) delete(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/sandboxes/"+id, nil, nil)
	if errors.Is(err, errSandboxGone) {
		return nil
	}
	return err
}

func (c *sandboxClient) exec(ctx context.Context, id, command string, timeoutSeconds int) (execResult, error) {
	var res execResult
	err := c.do(ctx, "POST", "/sandboxes/"+id+"/exec",
		jsonBody(map[string]any{"command": command, "timeout_seconds": timeoutSeconds}), &res)
	return res, err
}

func (c *sandboxClient) writeFile(ctx context.Context, id, path, content string) error {
	return c.do(ctx, "PUT", "/sandboxes/"+id+"/files?path="+url.QueryEscape(path), strings.NewReader(content), nil)
}

func (c *sandboxClient) readFile(ctx context.Context, id, path string) ([]byte, error) {
	var data []byte
	err := c.do(ctx, "GET", "/sandboxes/"+id+"/files?path="+url.QueryEscape(path), nil, &data)
	return data, err
}

func (c *sandboxClient) tree(ctx context.Context, id, path string) ([]treeEntry, error) {
	var entries []treeEntry
	err := c.do(ctx, "GET", "/sandboxes/"+id+"/tree?path="+url.QueryEscape(path), nil, &entries)
	return entries, err
}

func (c *sandboxClient) decisions(ctx context.Context, id string) ([]egressDecision, error) {
	var d []egressDecision
	err := c.do(ctx, "GET", "/sandboxes/"+id+"/egress/decisions", nil, &d)
	return d, err
}
