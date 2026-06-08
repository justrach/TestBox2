// Command gateway is a multi-tenant auth + metering proxy that sits in front of
// the CubeSandbox E2B-compatible API (cube-api, :3000).
//
// It turns a single CubeSandbox node into a multi-tenant substrate:
//   - authenticates each request by API key -> tenant
//   - enforces per-tenant concurrency + resource quotas on sandbox creation
//   - stamps metadata.tenant on every sandbox (for downstream filtering)
//   - meters sandbox lifetime (create -> destroy) into a JSONL ledger that
//     billing can aggregate (sandbox-seconds, vCPU-seconds, GB-seconds)
//
// Everything else is transparently reverse-proxied to the upstream API, so the
// stock E2B/cubesandbox SDKs work unchanged: point them at the gateway and set
// their API key to the tenant key.
//
// Stdlib only — builds to a single static binary (CGO_ENABLED=0).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ---- config ----

type Tenant struct {
	Key          string `json:"key"`
	ID           string `json:"id"`
	MaxSandboxes int    `json:"max_sandboxes"`  // 0 = unlimited
	MaxCPUCount  int    `json:"max_cpu_count"`  // 0 = no clamp
	MaxMemoryMB  int    `json:"max_memory_mb"`  // 0 = no clamp
}

type tenantsFile struct {
	Tenants []Tenant `json:"tenants"`
}

// ---- usage ledger ----

type event struct {
	Type      string  `json:"type"` // "open" | "close"
	Tenant    string  `json:"tenant"`
	SandboxID string  `json:"sandbox_id"`
	Template  string  `json:"template,omitempty"`
	CPU       int     `json:"cpu,omitempty"`
	MemMB     int     `json:"mem_mb,omitempty"`
	TS        int64   `json:"ts"` // unix seconds
}

type openInterval struct {
	Tenant   string
	Template string
	CPU      int
	MemMB    int
	Start    int64
}

// usage accumulators per tenant (closed intervals only; open computed live)
type acc struct {
	SandboxSeconds float64 `json:"sandbox_seconds"`
	VCPUSeconds    float64 `json:"vcpu_seconds"`
	GBSeconds      float64 `json:"gb_seconds"`
	Sandboxes      int     `json:"sandboxes_total"`
}

type Ledger struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	open   map[string]openInterval // sandboxID -> open
	closed map[string]*acc         // tenant -> closed totals
}

func newLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path, open: map[string]openInterval{}, closed: map[string]*acc{}}
	if err := l.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *Ledger) accFor(t string) *acc {
	a := l.closed[t]
	if a == nil {
		a = &acc{}
		l.closed[t] = a
	}
	return a
}

// replay rebuilds open intervals + closed totals from the JSONL on startup.
func (l *Ledger) replay() error {
	b, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Type {
		case "open":
			l.open[e.SandboxID] = openInterval{e.Tenant, e.Template, e.CPU, e.MemMB, e.TS}
			l.accFor(e.Tenant).Sandboxes++
		case "close":
			if iv, ok := l.open[e.SandboxID]; ok {
				l.addClosed(iv, e.TS)
				delete(l.open, e.SandboxID)
			}
		}
	}
	return nil
}

func (l *Ledger) addClosed(iv openInterval, end int64) {
	dur := float64(end - iv.Start)
	if dur < 0 {
		dur = 0
	}
	a := l.accFor(iv.Tenant)
	a.SandboxSeconds += dur
	a.VCPUSeconds += dur * float64(max1(iv.CPU))
	a.GBSeconds += dur * (float64(iv.MemMB) / 1024.0)
}

func (l *Ledger) write(e event) {
	if l.f == nil {
		return
	}
	b, _ := json.Marshal(e)
	l.f.Write(append(b, '\n'))
	l.f.Sync()
}

func (l *Ledger) openCount(tenant string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, iv := range l.open {
		if iv.Tenant == tenant {
			n++
		}
	}
	return n
}

func (l *Ledger) recordOpen(iv openInterval, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open[id] = iv
	l.accFor(iv.Tenant).Sandboxes++
	l.write(event{"open", iv.Tenant, id, iv.Template, iv.CPU, iv.MemMB, iv.Start})
}

func (l *Ledger) recordClose(id string, ts int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	iv, ok := l.open[id]
	if !ok {
		return
	}
	l.addClosed(iv, ts)
	delete(l.open, id)
	l.write(event{Type: "close", Tenant: iv.Tenant, SandboxID: id, TS: ts})
}

// usageSnapshot merges closed totals with live (still-open) contributions.
func (l *Ledger) usageSnapshot(now int64) map[string]*acc {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]*acc{}
	for t, a := range l.closed {
		c := *a
		out[t] = &c
	}
	for _, iv := range l.open {
		a := out[iv.Tenant]
		if a == nil {
			a = &acc{}
			out[iv.Tenant] = a
		}
		dur := float64(now - iv.Start)
		if dur < 0 {
			dur = 0
		}
		a.SandboxSeconds += dur
		a.VCPUSeconds += dur * float64(max1(iv.CPU))
		a.GBSeconds += dur * (float64(iv.MemMB) / 1024.0)
	}
	return out
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// ---- server ----

type Gateway struct {
	tenants  map[string]Tenant // key -> tenant
	byID     map[string]Tenant // id -> tenant
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	ledger   *Ledger
	admin    string
	client   *http.Client
}

func (g *Gateway) tenantFor(r *http.Request) (Tenant, bool) {
	key := r.Header.Get("X-API-KEY")
	if key == "" {
		key = r.Header.Get("X-API-Key")
	}
	if key == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			key = strings.TrimPrefix(a, "Bearer ")
		}
	}
	t, ok := g.tenants[key]
	return t, ok
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
		return
	case strings.HasPrefix(r.URL.Path, "/admin/"):
		g.handleAdmin(w, r)
		return
	}

	t, ok := g.tenantFor(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}

	// Intercept sandbox create / destroy; everything else is transparent.
	if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
		g.handleCreate(w, r, t)
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/") {
		id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
		if id != "" && !strings.Contains(id, "/") {
			g.proxy.ServeHTTP(w, r)
			g.ledger.recordClose(id, time.Now().Unix())
			return
		}
	}
	g.proxy.ServeHTTP(w, r)
}

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request, t Tenant) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	r.Body.Close()

	var m map[string]any
	if len(bytes.TrimSpace(body)) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Quota: concurrency
	if t.MaxSandboxes > 0 && g.ledger.openCount(t.ID) >= t.MaxSandboxes {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("tenant %s sandbox quota reached (%d)", t.ID, t.MaxSandboxes))
		return
	}

	// Resource clamps
	cpu := asInt(m["cpuCount"])
	if t.MaxCPUCount > 0 && (cpu == 0 || cpu > t.MaxCPUCount) {
		cpu = t.MaxCPUCount
		m["cpuCount"] = cpu
	}
	mem := asInt(m["memoryMB"])
	if t.MaxMemoryMB > 0 && (mem == 0 || mem > t.MaxMemoryMB) {
		mem = t.MaxMemoryMB
		m["memoryMB"] = mem
	}

	// Tag tenant in metadata
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["tenant"] = t.ID
	m["metadata"] = meta

	tmpl, _ := m["templateID"].(string)
	newBody, _ := json.Marshal(m)

	// Forward to upstream
	req, _ := http.NewRequest(http.MethodPost, g.upstream.String()+"/sandboxes", bytes.NewReader(newBody))
	copyHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(newBody))

	resp, err := g.client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Record open on success
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var rb struct {
			SandboxID string `json:"sandboxID"`
		}
		if json.Unmarshal(respBody, &rb) == nil && rb.SandboxID != "" {
			g.ledger.recordOpen(openInterval{
				Tenant: t.ID, Template: tmpl, CPU: cpu, MemMB: mem, Start: time.Now().Unix(),
			}, rb.SandboxID)
		}
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (g *Gateway) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if g.admin == "" || r.Header.Get("X-Admin-Token") != g.admin {
		writeErr(w, http.StatusUnauthorized, "admin token required")
		return
	}
	switch r.URL.Path {
	case "/admin/usage":
		snap := g.ledger.usageSnapshot(time.Now().Unix())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"as_of": time.Now().UTC().Format(time.RFC3339),
			"usage": snap,
			"open":  g.openByTenant(),
		})
	case "/admin/tenants":
		out := []map[string]any{}
		for _, t := range g.byID {
			out = append(out, map[string]any{
				"id": t.ID, "max_sandboxes": t.MaxSandboxes,
				"max_cpu_count": t.MaxCPUCount, "max_memory_mb": t.MaxMemoryMB,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tenants": out})
	default:
		writeErr(w, http.StatusNotFound, "unknown admin endpoint")
	}
}

func (g *Gateway) openByTenant() map[string]int {
	g.ledger.mu.Lock()
	defer g.ledger.mu.Unlock()
	out := map[string]int{}
	for _, iv := range g.ledger.open {
		out[iv.Tenant]++
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func main() {
	addr := flag.String("addr", env("GATEWAY_ADDR", ":8088"), "listen address")
	up := flag.String("upstream", env("CUBE_UPSTREAM", "http://127.0.0.1:3000"), "cube-api upstream")
	tenantsPath := flag.String("tenants", env("GATEWAY_TENANTS", "tenants.json"), "tenants config file")
	usagePath := flag.String("usage", env("GATEWAY_USAGE_LOG", "usage.jsonl"), "usage ledger (JSONL)")
	admin := flag.String("admin-token", env("GATEWAY_ADMIN_TOKEN", ""), "admin API token")
	flag.Parse()

	tb, err := os.ReadFile(*tenantsPath)
	if err != nil {
		log.Fatalf("read tenants: %v", err)
	}
	var tf tenantsFile
	if err := json.Unmarshal(tb, &tf); err != nil {
		log.Fatalf("parse tenants: %v", err)
	}
	tenants := map[string]Tenant{}
	byID := map[string]Tenant{}
	for _, t := range tf.Tenants {
		if t.Key == "" || t.ID == "" {
			log.Fatalf("tenant entries need key and id")
		}
		tenants[t.Key] = t
		byID[t.ID] = t
	}

	upURL, err := url.Parse(*up)
	if err != nil {
		log.Fatalf("upstream url: %v", err)
	}
	ledger, err := newLedger(*usagePath)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}

	g := &Gateway{
		tenants:  tenants,
		byID:     byID,
		upstream: upURL,
		proxy:    httputil.NewSingleHostReverseProxy(upURL),
		ledger:   ledger,
		admin:    *admin,
		client:   &http.Client{Timeout: 60 * time.Second},
	}

	log.Printf("cube gateway: listening %s -> %s | tenants=%d | usage=%s",
		*addr, upURL, len(tenants), *usagePath)
	srv := &http.Server{Addr: *addr, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
