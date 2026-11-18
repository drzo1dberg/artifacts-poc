package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	guestAPIPort = 1024
	bootTimeout  = 15 * time.Second
	kernelArgs   = "console=ttyS0 reboot=k panic=1 pci=off rw quiet init=/sbin/sandbox-agent"
)

// vm is one Firecracker process. Everything it needs lives in dir, at fixed relative names,
// because a snapshot can only be restored when the disk and the vsock socket are where they were.
type vm struct {
	dir     string
	cmd     *exec.Cmd
	exited  chan struct{}
	console *os.File
}

func (v *vm) path(name string) string { return filepath.Join(v.dir, name) }

// startFirecracker launches the VMM. It exposes its own API on a Unix socket, which is how we
// configure, pause, snapshot, and restore it.
func startFirecracker(dir string) (*vm, error) {
	v := &vm{dir: dir, exited: make(chan struct{})}
	os.Remove(v.path("api.sock"))
	// Firecracker binds the vsock socket itself and refuses if the file is still there.
	os.Remove(v.path("v.sock"))
	console, err := os.OpenFile(v.path("console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	v.console = console
	v.cmd = exec.Command("firecracker", "--api-sock", "api.sock", "--level", "Warning")
	v.cmd.Dir = dir
	v.cmd.Stdout, v.cmd.Stderr = console, console
	if err := v.cmd.Start(); err != nil {
		console.Close()
		return nil, err
	}
	go func() {
		v.cmd.Wait()
		console.Close()
		close(v.exited)
	}()
	if err := waitFor(func() bool { _, err := os.Stat(v.path("api.sock")); return err == nil }); err != nil {
		v.kill()
		return nil, errors.New("firecracker did not open its API socket")
	}
	return v, nil
}

// boot configures a fresh VM from the disk image and starts it.
func boot(dir string, vcpus, memMiB int) (*vm, error) {
	v, err := startFirecracker(dir)
	if err != nil {
		return nil, err
	}
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/boot-source", map[string]any{"kernel_image_path": kernelPath, "boot_args": kernelArgs}},
		{"PUT", "/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "rootfs.ext4", "is_root_device": true, "is_read_only": false}},
		{"PUT", "/machine-config", map[string]any{"vcpu_count": vcpus, "mem_size_mib": memMiB}},
		// guest_cid 3 is fine for every VM: each one has its own vsock device and socket file.
		{"PUT", "/vsock", map[string]any{"guest_cid": 3, "uds_path": "v.sock"}},
		{"PUT", "/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if err := v.api(s.method, s.path, s.body); err != nil {
			v.kill()
			return nil, err
		}
	}
	if err := v.waitReady(); err != nil {
		v.kill()
		return nil, err
	}
	return v, nil
}

// snapshot pauses the VM and writes memory and device state to disk, then ends the process.
// Together with the disk file this is everything resume needs.
func (v *vm) snapshot() error {
	if err := v.api("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return err
	}
	err := v.api("PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": "vm.snap", "mem_file_path": "vm.mem",
	})
	v.kill()
	return err
}

// restore starts a new Firecracker process from the snapshot files. Open vsock connections do not
// survive this, but the agent's listening socket does, so the next request simply reconnects.
func restore(dir string) (*vm, error) {
	v, err := startFirecracker(dir)
	if err != nil {
		return nil, err
	}
	err = v.api("PUT", "/snapshot/load", map[string]any{
		"snapshot_path": "vm.snap",
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": "vm.mem"},
		"resume_vm":     true,
	})
	if err == nil {
		err = v.waitReady()
	}
	if err != nil {
		v.kill()
		return nil, err
	}
	os.Remove(v.path("vm.mem"))
	os.Remove(v.path("vm.snap"))
	return v, nil
}

func (v *vm) kill() {
	if v.cmd.Process != nil {
		v.cmd.Process.Kill()
	}
	<-v.exited
}

func (v *vm) alive() bool {
	select {
	case <-v.exited:
		return false
	default:
		return true
	}
}

func (v *vm) waitReady() error {
	deadline := time.Now().Add(bootTimeout)
	for time.Now().Before(deadline) {
		if !v.alive() {
			return fmt.Errorf("VM stopped while booting, see %s", v.path("console.log"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		res, err := v.guest(ctx, "GET", "/health", nil)
		cancel()
		if err == nil {
			res.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("guest agent did not answer in time")
}

// api calls the Firecracker API on its Unix socket.
func (v *vm) api(method, path string, body any) error {
	client := http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", v.path("api.sock"))
		},
	}}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, "http://firecracker"+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(res.Body)
		return fmt.Errorf("firecracker %s %s: %s %s", method, path, res.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// guest sends one HTTP request to the agent in the VM. Firecracker maps vsock to a Unix socket on
// the host: connect, write "CONNECT <port>", read "OK ...", and the stream goes to the guest port.
func (v *vm) guest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	client := http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", v.path("v.sock"))
			if err != nil {
				return nil, err
			}
			if deadline, ok := ctx.Deadline(); ok {
				conn.SetDeadline(deadline)
			}
			fmt.Fprintf(conn, "CONNECT %d\n", guestAPIPort)
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "OK ") {
				conn.Close()
				return nil, fmt.Errorf("vsock handshake failed: %q", line)
			}
			conn.SetDeadline(time.Time{})
			return &bufferedConn{Conn: conn, r: reader}, nil
		},
	}}
	req, err := http.NewRequestWithContext(ctx, method, "http://guest"+path, body)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// bufferedConn keeps bytes the handshake reader already pulled off the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func waitFor(ok func() bool) error {
	for range 200 {
		if ok() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timeout")
}
