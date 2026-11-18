// sandbox-agent runs as PID 1 inside every microVM. It is the only way into the VM: the host reaches
// it over vsock, there is no network card. It runs commands and reads and writes files for sandboxd.
//
// It also forwards outbound traffic. Commands get HTTP_PROXY and HTTPS_PROXY pointing to a listener
// on 127.0.0.1:3128, which pipes every connection over vsock to the egress proxy in sandboxd. That
// proxy applies the egress policy, so nothing leaves the VM without a decision on the host.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

const (
	apiPort       = 1024 // host to guest: this HTTP API
	egressPort    = 1025 // guest to host: the egress proxy in sandboxd
	hostCID       = 2    // vsock address of the host, fixed by the virtio spec
	proxyAddr     = "127.0.0.1:3128"
	maxOutput     = 1 << 20
	maxFileBytes  = 10 << 20
	maxTreeFiles  = 500
	defaultTimout = 60 * time.Second
	maxTimeout    = 10 * time.Minute
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("sandbox-agent: ")
	if os.Getpid() == 1 {
		setupSystem()
		runInit()
		return
	}
	go forwardEgress()

	ln, err := vsock.Listen(apiPort, nil)
	if err != nil {
		log.Fatal("listen on vsock: ", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /exec", execCommand)
	mux.HandleFunc("GET /files", readFile)
	mux.HandleFunc("PUT /files", writeFile)
	mux.HandleFunc("GET /tree", listTree)
	log.Println("ready")
	log.Fatal(http.Serve(ln, mux))
}

// setupSystem does what an init system would do. The kernel mounts /dev itself (DEVTMPFS_MOUNT).
func setupSystem() {
	mounts := []struct{ source, target, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"tmpfs", "/run", "tmpfs"},
		{"tmpfs", "/dev/shm", "tmpfs"},
	}
	for _, m := range mounts {
		os.MkdirAll(m.target, 0o755)
		if err := unix.Mount(m.source, m.target, m.fstype, 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
			log.Printf("mount %s: %v", m.target, err)
		}
	}
	os.Chmod("/tmp", 0o1777)
	unix.Sethostname([]byte("sandbox"))
	if err := loopbackUp(); err != nil {
		log.Println("loopback:", err)
	}
	os.MkdirAll("/workspace", 0o755)
}

// runInit keeps PID 1 small: it starts the agent as a child and reaps every process that ends.
// Orphans of commands land at PID 1, so they never become zombies. Waiting here cannot steal an
// exit status from the agent, because the commands the agent runs are its own children.
func runInit() {
	self, _ := os.Executable()
	agent := exec.Command(self)
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	if err := agent.Start(); err != nil {
		log.Fatal("start agent: ", err)
	}
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil || pid == agent.Process.Pid {
			// Without an agent the VM is unreachable. Exiting PID 1 panics the kernel, and with
			// reboot=k and panic=1 on the command line Firecracker then stops.
			log.Fatal("agent stopped: ", status.ExitStatus())
		}
	}
}

// loopbackUp is "ip link set lo up". The proxy listener needs a working 127.0.0.1.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// forwardEgress accepts local proxy connections and pipes each one to the host.
func forwardEgress() {
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		log.Println("egress listener:", err)
		return
	}
	for {
		local, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer local.Close()
			remote, err := vsock.Dial(hostCID, egressPort, nil)
			if err != nil {
				return
			}
			defer remote.Close()
			go io.Copy(remote, local)
			io.Copy(local, remote)
		}()
	}
}

type execRequest struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type execResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// cappedBuffer keeps the first maxOutput bytes and drops the rest, so a noisy command cannot fill memory.
type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func execCommand(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	timeout := defaultTimout
	if req.TimeoutSeconds > 0 {
		timeout = min(time.Duration(req.TimeoutSeconds)*time.Second, maxTimeout)
	}
	if req.Cwd == "" {
		req.Cwd = "/workspace"
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var stdout, stderr cappedBuffer
	cmd := exec.Command("/bin/sh", "-c", req.Command)
	cmd.Dir = req.Cwd
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"LANG=C.UTF-8",
		"HTTP_PROXY=http://" + proxyAddr, "HTTPS_PROXY=http://" + proxyAddr,
		"http_proxy=http://" + proxyAddr, "https_proxy=http://" + proxyAddr,
		"NO_PROXY=localhost,127.0.0.1",
	}
	// Own session and process group: a timeout kills the whole tree, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		writeJSON(w, execResult{ExitCode: 127, Stderr: err.Error()})
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		timedOut = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	res := execResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		TimedOut:   timedOut,
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.ExitCode = -1
		res.Stderr += err.Error()
	}
	writeJSON(w, res)
}

func readFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.IsDir() || info.Size() > maxFileBytes {
		http.Error(w, "not a regular file of at most 10 MiB", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

func writeFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFileBytes))
	if err != nil {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type treeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// listTree returns all regular files below a folder, with paths relative to it.
func listTree(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("path")
	if !filepath.IsAbs(root) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	entries := []treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(entries) == maxTreeFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		entries = append(entries, treeEntry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, entries)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
