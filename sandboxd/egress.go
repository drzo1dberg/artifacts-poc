package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The egress proxy is the only way out of a VM. The agent in the guest forwards every proxy
// connection over vsock, Firecracker turns it into a connection on the Unix socket v.sock_1025
// in the sandbox folder, and this file decides what happens to it.
//
// Like ACA Sandboxes, the policy is per sandbox, the first matching rule wins, and without a
// match the default action applies. HTTPS is only inspected up to the host name in CONNECT, as
// with trafficInspection "Partial". Header changes therefore only work for plain HTTP.

const (
	egressSocket   = "v.sock_1025"
	maxDecisions   = 100
	egressDialTime = 10 * time.Second
)

type egressRule struct {
	// Host matches the name exactly, or with a leading "*." any subdomain.
	Host   string `json:"host"`
	Action string `json:"action"` // Allow, Deny, or Transform
	// SetHeaders applies to Transform. A value of {secret:NAME} is replaced with the environment
	// variable SANDBOX_SECRET_NAME of sandboxd, so the secret never enters the VM.
	SetHeaders map[string]string `json:"set_headers,omitempty"`
}

type egressPolicy struct {
	DefaultAction string       `json:"default_action"` // Allow or Deny
	Rules         []egressRule `json:"rules"`
}

type egressDecision struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Action string    `json:"action"`
	Reason string    `json:"reason"`
}

var (
	validHostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	secretRef        = regexp.MustCompile(`\{secret:([A-Z0-9_]+)\}`)
)

func denyAll() egressPolicy { return egressPolicy{DefaultAction: "Deny", Rules: []egressRule{}} }

func (p egressPolicy) validate() error {
	if p.DefaultAction != "Allow" && p.DefaultAction != "Deny" {
		return errors.New("default_action must be Allow or Deny")
	}
	for _, r := range p.Rules {
		if !validHostPattern.MatchString(r.Host) {
			return fmt.Errorf("invalid host pattern %q", r.Host)
		}
		switch r.Action {
		case "Allow", "Deny":
		case "Transform":
			for _, v := range r.SetHeaders {
				for _, m := range secretRef.FindAllStringSubmatch(v, -1) {
					if os.Getenv("SANDBOX_SECRET_"+m[1]) == "" {
						return fmt.Errorf("unknown secret %q", m[1])
					}
				}
			}
		default:
			return fmt.Errorf("action must be Allow, Deny, or Transform, got %q", r.Action)
		}
	}
	return nil
}

// decide returns the first matching rule, or a rule that stands for the default action.
func (p egressPolicy) decide(host string) (egressRule, string) {
	host = strings.ToLower(host)
	for i, r := range p.Rules {
		if r.Host == host || (strings.HasPrefix(r.Host, "*.") && strings.HasSuffix(host, r.Host[1:])) {
			return r, fmt.Sprintf("rule %d (%s)", i, r.Host)
		}
	}
	return egressRule{Action: p.DefaultAction}, "default action"
}

// serveEgress accepts the connections the guest opens until the listener closes.
func (sb *sandbox) serveEgress(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go sb.handleEgress(conn)
	}
}

func (sb *sandbox) handleEgress(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	host, port := req.URL.Hostname(), req.URL.Port()
	if req.Method == http.MethodConnect {
		host, port, _ = net.SplitHostPort(req.Host)
	}
	if port == "" {
		port = "80"
	}
	rule, reason := sb.policy().decide(host)
	allowed := rule.Action != "Deny"
	if allowed && port != "80" && port != "443" {
		allowed, reason = false, "only ports 80 and 443"
	}
	if allowed && req.Method != http.MethodConnect && req.URL.Scheme != "http" {
		allowed, reason = false, "plain proxy requests must use http"
	}
	if !allowed {
		sb.record(req.Method, host, false, reason)
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\negress denied by policy: %s\n", reason)
		return
	}

	dialer := &net.Dialer{Timeout: egressDialTime, Control: refusePrivate}
	if req.Method == http.MethodConnect {
		upstream, err := dialer.Dial("tcp", net.JoinHostPort(host, port))
		if err != nil {
			sb.record(req.Method, host, false, err.Error())
			fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n%v\n", err)
			return
		}
		defer upstream.Close()
		sb.record(req.Method, host, true, reason)
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(upstream, reader)
		io.Copy(conn, upstream)
		return
	}

	// Plain HTTP: forward a single request, with injected headers for Transform.
	for name, value := range rule.SetHeaders {
		req.Header.Set(name, secretRef.ReplaceAllStringFunc(value, func(ref string) string {
			return os.Getenv("SANDBOX_SECRET_" + secretRef.FindStringSubmatch(ref)[1])
		}))
	}
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	req.Close = true
	transport := &http.Transport{DialContext: dialer.DialContext, Proxy: nil}
	res, err := transport.RoundTrip(req)
	if err != nil {
		sb.record(req.Method, host, false, err.Error())
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n%v\n", err)
		return
	}
	defer res.Body.Close()
	sb.record(req.Method, host, true, reason)
	res.Close = true
	res.Write(conn)
}

// refusePrivate runs after DNS resolution, right before the connect. It keeps the VM away from
// the compose network, the host, and cloud metadata endpoints even when the policy allows all,
// and a DNS name that resolves to such an address cannot get around it.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, _ := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("destination %s is not public", host)
	}
	return nil
}

func (sb *sandbox) record(method, host string, allowed bool, reason string) {
	action := "Allow"
	if !allowed {
		action = "Deny"
	}
	log.Printf("egress %s: %s %s -> %s (%s)", sb.ID, method, host, action, reason)
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.decisions = append(sb.decisions, egressDecision{Time: time.Now().UTC(), Method: method, Host: host, Action: action, Reason: reason})
	if len(sb.decisions) > maxDecisions {
		sb.decisions = sb.decisions[len(sb.decisions)-maxDecisions:]
	}
}

func (sb *sandbox) policy() egressPolicy {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.egress
}
