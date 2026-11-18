// sandboxd stands in for Azure Container Apps Sandboxes. Every sandbox is a Firecracker microVM
// with its own kernel and its own copy of a disk image. The caller runs commands and moves files
// through this API, and nothing in the VM can reach the network except through the egress proxy.
//
// The operations follow ACA Sandboxes: create from a disk, exec, files, stop with a memory
// snapshot, resume, delete, an egress policy per sandbox, and idle based auto suspend and delete.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dataDir         = "/data"
	disksSrc        = "/opt/disks"
	kernelPath      = "/opt/kernel/vmlinux"
	diskSize        = "2048M"
	lifecycleTick   = 2 * time.Second
	defaultVCPUs    = 1
	defaultMemMiB   = 512
	maxMemMiB       = 2048
	maxVCPUs        = 2
	maxRequestBytes = 11 << 20
)

const (
	stateRunning = "Running"
	stateStopped = "Stopped"
	stateFailed  = "Failed"
)

var validLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

type lifecycle struct {
	// AutoSuspendSeconds stops an idle sandbox with a memory snapshot. Zero turns it off.
	AutoSuspendSeconds int `json:"auto_suspend_seconds"`
	// AutoDeleteSeconds deletes a sandbox that was idle this long, running or stopped. Zero turns it off.
	AutoDeleteSeconds int `json:"auto_delete_seconds"`
}

type sandbox struct {
	ID        string            `json:"id"`
	Disk      string            `json:"disk"`
	Labels    map[string]string `json:"labels"`
	State     string            `json:"state"`
	VCPUs     int               `json:"vcpus"`
	MemMiB    int               `json:"memory_mib"`
	Lifecycle lifecycle         `json:"lifecycle"`
	CreatedAt time.Time         `json:"created_at"`
	LastUsed  time.Time         `json:"last_activity_at"`

	// mu guards the fields above and below. op serializes boot, stop, resume, and delete, which
	// take long and must not overlap. Lock order is op, then mu.
	mu        sync.Mutex
	op        sync.Mutex
	dir       string
	vm        *vm
	inflight  int
	egress    egressPolicy
	decisions []egressDecision
	egressLn  net.Listener
	deleted   bool
}

type server struct {
	mu        sync.Mutex
	sandboxes map[string]*sandbox
	disks     []string
	max       int
	token     string
}

func main() {
	token := os.Getenv("SANDBOXD_TOKEN")
	if token == "" {
		log.Fatal("SANDBOXD_TOKEN is required")
	}
	max := 4
	if v, err := strconv.Atoi(os.Getenv("MAX_SANDBOXES")); err == nil && v > 0 {
		max = v
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		log.Fatal("/dev/kvm is missing: sandboxd needs hardware virtualization")
	}
	// Firecracker processes do not survive a restart of sandboxd, so neither do their sandboxes.
	os.RemoveAll(filepath.Join(dataDir, "sandboxes"))
	disks, err := buildDisks()
	if err != nil {
		log.Fatal("build disk images: ", err)
	}
	s := &server{sandboxes: map[string]*sandbox{}, disks: disks, max: max, token: token}
	go s.runLifecycle()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /disks", s.auth(s.listDisks))
	mux.HandleFunc("POST /sandboxes", s.auth(s.create))
	mux.HandleFunc("GET /sandboxes", s.auth(s.list))
	mux.HandleFunc("GET /sandboxes/{id}", s.withSandbox(s.get))
	mux.HandleFunc("DELETE /sandboxes/{id}", s.withSandbox(s.delete))
	mux.HandleFunc("POST /sandboxes/{id}/stop", s.withSandbox(s.stop))
	mux.HandleFunc("POST /sandboxes/{id}/resume", s.withSandbox(s.resume))
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.withSandbox(s.proxyToGuest("POST", "/exec")))
	mux.HandleFunc("GET /sandboxes/{id}/files", s.withSandbox(s.proxyToGuest("GET", "/files")))
	mux.HandleFunc("PUT /sandboxes/{id}/files", s.withSandbox(s.proxyToGuest("PUT", "/files")))
	mux.HandleFunc("GET /sandboxes/{id}/tree", s.withSandbox(s.proxyToGuest("GET", "/tree")))
	mux.HandleFunc("GET /sandboxes/{id}/egress", s.withSandbox(s.getEgress))
	mux.HandleFunc("PUT /sandboxes/{id}/egress", s.withSandbox(s.setEgress))
	mux.HandleFunc("GET /sandboxes/{id}/egress/decisions", s.withSandbox(s.getDecisions))
	mux.HandleFunc("PUT /sandboxes/{id}/lifecycle", s.withSandbox(s.setLifecycle))

	srv := &http.Server{Addr: ":8090", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("sandboxd listening on :8090, disks %v, at most %d sandboxes", disks, max)
	log.Fatal(srv.ListenAndServe())
}

// buildDisks turns every folder in /opt/disks into an ext4 image. mkfs.ext4 -d copies a folder
// into a new file system without root rights or loop devices. Each sandbox later gets its own copy.
func buildDisks() ([]string, error) {
	entries, err := os.ReadDir(disksSrc)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(dataDir, "disks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tmp := filepath.Join(dir, e.Name()+".ext4.tmp")
		os.Remove(tmp)
		start := time.Now()
		out, err := exec.Command("mkfs.ext4", "-q", "-F", "-L", "rootfs", "-d", filepath.Join(disksSrc, e.Name()), tmp, diskSize).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("mkfs %s: %v: %s", e.Name(), err, out)
		}
		if err := os.Rename(tmp, filepath.Join(dir, e.Name()+".ext4")); err != nil {
			return nil, err
		}
		log.Printf("disk %s built in %s", e.Name(), time.Since(start).Round(time.Millisecond))
		names = append(names, e.Name())
	}
	return names, nil
}

// --- Handlers ---

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next(w, r)
	}
}

func (s *server) withSandbox(next func(http.ResponseWriter, *http.Request, *sandbox)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		sb := s.sandboxes[r.PathValue("id")]
		s.mu.Unlock()
		if sb == nil {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		next(w, r, sb)
	})
}

func (s *server) listDisks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.disks)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disk      string            `json:"disk"`
		Labels    map[string]string `json:"labels"`
		VCPUs     int               `json:"vcpus"`
		MemMiB    int               `json:"memory_mib"`
		Lifecycle lifecycle         `json:"lifecycle"`
		Egress    *egressPolicy     `json:"egress"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !slices.Contains(s.disks, req.Disk) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown disk %q, available: %v", req.Disk, s.disks))
		return
	}
	for k, v := range req.Labels {
		if !validLabel.MatchString(k) || len(v) > 128 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid label %q", k))
			return
		}
	}
	req.VCPUs = orDefault(req.VCPUs, defaultVCPUs)
	req.MemMiB = orDefault(req.MemMiB, defaultMemMiB)
	if req.VCPUs < 1 || req.VCPUs > maxVCPUs || req.MemMiB < 128 || req.MemMiB > maxMemMiB {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("vcpus must be 1-%d and memory_mib 128-%d", maxVCPUs, maxMemMiB))
		return
	}
	if req.Lifecycle.AutoSuspendSeconds < 0 || req.Lifecycle.AutoDeleteSeconds < 0 {
		writeError(w, http.StatusBadRequest, "lifecycle values must not be negative")
		return
	}
	// Like ACA, a sandbox has unrestricted egress unless a policy says otherwise. This POC flips
	// that default: a sandbox without a policy gets none at all.
	policy := denyAll()
	if req.Egress != nil {
		policy = *req.Egress
	}
	if err := policy.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := newID()
	sb := &sandbox{
		ID: id, Disk: req.Disk, Labels: req.Labels, VCPUs: req.VCPUs, MemMiB: req.MemMiB,
		Lifecycle: req.Lifecycle, CreatedAt: time.Now().UTC(), LastUsed: time.Now().UTC(),
		dir: filepath.Join(dataDir, "sandboxes", id), egress: policy,
	}
	if sb.Labels == nil {
		sb.Labels = map[string]string{}
	}
	s.mu.Lock()
	if len(s.sandboxes) >= s.max {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d sandboxes, delete one first", s.max))
		return
	}
	s.sandboxes[id] = sb
	s.mu.Unlock()

	start := time.Now()
	if err := sb.boot(); err != nil {
		s.remove(sb)
		log.Printf("create %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "boot failed: "+err.Error())
		return
	}
	log.Printf("created %s from %s in %s", id, req.Disk, time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusCreated, sb.view())
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	// ?label=key=value filters, like "aca sandbox list -l".
	key, value, filtered := strings.Cut(r.URL.Query().Get("label"), "=")
	s.mu.Lock()
	all := make([]*sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		all = append(all, sb)
	}
	s.mu.Unlock()
	result := []sandboxView{}
	for _, sb := range all {
		v := sb.view()
		if filtered && v.Labels[key] != value {
			continue
		}
		result = append(result, v)
	}
	slices.SortFunc(result, func(a, b sandboxView) int { return a.CreatedAt.Compare(b.CreatedAt) })
	writeJSON(w, http.StatusOK, result)
}

func (s *server) get(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	writeJSON(w, http.StatusOK, sb.view())
}

func (s *server) delete(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	s.remove(sb)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) stop(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	if err := sb.stop(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb.view())
}

func (s *server) resume(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	if err := sb.ensureRunning(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb.view())
}

// proxyToGuest forwards a request to the agent in the VM. A stopped sandbox resumes first, the
// same way a command wakes up a suspended ACA sandbox.
func (s *server) proxyToGuest(method, path string) func(http.ResponseWriter, *http.Request, *sandbox) {
	return func(w http.ResponseWriter, r *http.Request, sb *sandbox) {
		v, err := sb.acquire()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		defer sb.release()
		target := path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		res, err := v.guest(r.Context(), method, target, http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeError(w, http.StatusBadGateway, "guest agent: "+err.Error())
			return
		}
		defer res.Body.Close()
		if ct := res.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	}
}

func (s *server) getEgress(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	writeJSON(w, http.StatusOK, sb.policy())
}

func (s *server) setEgress(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	var p egressPolicy
	if !readJSON(w, r, &p) {
		return
	}
	if p.Rules == nil {
		p.Rules = []egressRule{}
	}
	if err := p.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sb.mu.Lock()
	sb.egress = p
	sb.mu.Unlock()
	writeJSON(w, http.StatusOK, p)
}

func (s *server) getDecisions(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	sb.mu.Lock()
	d := slices.Clone(sb.decisions)
	sb.mu.Unlock()
	if d == nil {
		d = []egressDecision{}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) setLifecycle(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	var l lifecycle
	if !readJSON(w, r, &l) {
		return
	}
	if l.AutoSuspendSeconds < 0 || l.AutoDeleteSeconds < 0 {
		writeError(w, http.StatusBadRequest, "lifecycle values must not be negative")
		return
	}
	sb.mu.Lock()
	sb.Lifecycle = l
	sb.mu.Unlock()
	writeJSON(w, http.StatusOK, sb.view())
}

// --- Sandbox lifecycle ---

// boot gives the sandbox its own copy of the disk image and starts the VM. On a copy-on-write file
// system such as btrfs, --reflink makes the copy instant. Elsewhere it is a sparse copy.
func (sb *sandbox) boot() error {
	sb.op.Lock()
	defer sb.op.Unlock()
	if err := os.MkdirAll(sb.dir, 0o700); err != nil {
		return err
	}
	disk := filepath.Join(dataDir, "disks", sb.Disk+".ext4")
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", disk, filepath.Join(sb.dir, "rootfs.ext4")).CombinedOutput(); err != nil {
		return fmt.Errorf("copy disk: %v: %s", err, out)
	}
	ln, err := net.Listen("unix", filepath.Join(sb.dir, egressSocket))
	if err != nil {
		return err
	}
	sb.egressLn = ln
	go sb.serveEgress(ln)
	v, err := boot(sb.dir, sb.VCPUs, sb.MemMiB)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	sb.vm, sb.State = v, stateRunning
	sb.mu.Unlock()
	return nil
}

// acquire makes sure the VM runs and counts the caller as active, so auto suspend leaves it alone.
func (sb *sandbox) acquire() (*vm, error) {
	if err := sb.ensureRunning(); err != nil {
		return nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.State != stateRunning {
		return nil, fmt.Errorf("sandbox is %s", sb.State)
	}
	sb.inflight++
	sb.LastUsed = time.Now().UTC()
	return sb.vm, nil
}

func (sb *sandbox) release() {
	sb.mu.Lock()
	sb.inflight--
	sb.LastUsed = time.Now().UTC()
	sb.mu.Unlock()
}

func (sb *sandbox) ensureRunning() error {
	sb.op.Lock()
	defer sb.op.Unlock()
	sb.mu.Lock()
	state, deleted := sb.currentState(), sb.deleted
	sb.mu.Unlock()
	switch {
	case deleted:
		return errors.New("sandbox was deleted")
	case state == stateRunning:
		return nil
	case state != stateStopped:
		return fmt.Errorf("sandbox is %s", state)
	}
	start := time.Now()
	v, err := restore(sb.dir)
	if err != nil {
		sb.mu.Lock()
		sb.State = stateFailed
		sb.mu.Unlock()
		return fmt.Errorf("resume: %w", err)
	}
	sb.mu.Lock()
	sb.vm, sb.State, sb.LastUsed = v, stateRunning, time.Now().UTC()
	sb.mu.Unlock()
	log.Printf("resumed %s in %s", sb.ID, time.Since(start).Round(time.Millisecond))
	return nil
}

func (sb *sandbox) stop() error {
	sb.op.Lock()
	defer sb.op.Unlock()
	sb.mu.Lock()
	state, inflight, v := sb.currentState(), sb.inflight, sb.vm
	sb.mu.Unlock()
	switch {
	case state == stateStopped:
		return nil
	case state != stateRunning:
		return fmt.Errorf("sandbox is %s", state)
	case inflight > 0:
		return errors.New("sandbox is busy, try again when the running request has finished")
	}
	start := time.Now()
	err := v.snapshot()
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if err != nil {
		sb.State = stateFailed
		return fmt.Errorf("snapshot: %w", err)
	}
	sb.State, sb.vm = stateStopped, nil
	log.Printf("stopped %s with a memory snapshot in %s", sb.ID, time.Since(start).Round(time.Millisecond))
	return nil
}

// currentState notices a VM that ended on its own, for example after a kernel panic. Needs mu.
func (sb *sandbox) currentState() string {
	if sb.State == stateRunning && (sb.vm == nil || !sb.vm.alive()) {
		sb.State = stateFailed
	}
	return sb.State
}

func (s *server) remove(sb *sandbox) {
	s.mu.Lock()
	delete(s.sandboxes, sb.ID)
	s.mu.Unlock()
	sb.op.Lock()
	defer sb.op.Unlock()
	sb.mu.Lock()
	v, ln := sb.vm, sb.egressLn
	sb.vm, sb.deleted, sb.State = nil, true, "Deleted"
	sb.mu.Unlock()
	if v != nil {
		v.kill()
	}
	if ln != nil {
		ln.Close()
	}
	os.RemoveAll(sb.dir)
	log.Printf("deleted %s", sb.ID)
}

// runLifecycle applies auto suspend and auto delete, like the lifecycle policy of an ACA sandbox.
func (s *server) runLifecycle() {
	for range time.Tick(lifecycleTick) {
		s.mu.Lock()
		all := make([]*sandbox, 0, len(s.sandboxes))
		for _, sb := range s.sandboxes {
			all = append(all, sb)
		}
		s.mu.Unlock()
		for _, sb := range all {
			sb.mu.Lock()
			idle := time.Since(sb.LastUsed)
			l, state, inflight := sb.Lifecycle, sb.currentState(), sb.inflight
			sb.mu.Unlock()
			switch {
			case inflight > 0:
			case l.AutoDeleteSeconds > 0 && idle > time.Duration(l.AutoDeleteSeconds)*time.Second:
				log.Printf("lifecycle: %s idle for %s, deleting", sb.ID, idle.Round(time.Second))
				s.remove(sb)
			case state == stateRunning && l.AutoSuspendSeconds > 0 && idle > time.Duration(l.AutoSuspendSeconds)*time.Second:
				if err := sb.stop(); err != nil {
					log.Printf("lifecycle: suspend %s: %v", sb.ID, err)
				}
			}
		}
	}
}

// --- Helpers ---

type sandboxView struct {
	ID        string            `json:"id"`
	Disk      string            `json:"disk"`
	Labels    map[string]string `json:"labels"`
	State     string            `json:"state"`
	VCPUs     int               `json:"vcpus"`
	MemMiB    int               `json:"memory_mib"`
	Lifecycle lifecycle         `json:"lifecycle"`
	CreatedAt time.Time         `json:"created_at"`
	LastUsed  time.Time         `json:"last_activity_at"`
}

func (sb *sandbox) view() sandboxView {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sandboxView{sb.ID, sb.Disk, sb.Labels, sb.currentState(), sb.VCPUs, sb.MemMiB, sb.Lifecycle, sb.CreatedAt, sb.LastUsed}
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}

func orDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
