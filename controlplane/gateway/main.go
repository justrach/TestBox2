// Command gateway is a multi-tenant auth + metering proxy that sits in front of
// the CubeSandbox E2B-compatible API (cube-api, :3000).
//
// It turns a single CubeSandbox node into a multi-tenant substrate:
//   - authenticates each request by API key -> tenant
//   - AUTHORIZES every sandbox-addressed path: a tenant may only see/touch its
//     own sandboxes (ownership is held in the in-process ledger)
//   - enforces per-tenant concurrency + resource quotas on sandbox creation
//   - stamps metadata.tenant on every sandbox (for ownership + list filtering)
//   - meters sandbox lifetime (create -> destroy) into a JSONL ledger that
//     billing can aggregate (sandbox-seconds, vCPU-seconds, GB-seconds)
//
// Authentication is NOT authorization: cube-api has no tenant model and serves
// every route at both "/" and "/cubeapi/v1/" (plus a /v2/sandboxes list), so a
// blind transparent proxy would let any authenticated tenant read the whole
// node or mutate shared state. This gateway therefore default-denies: only the
// tenant-scoped data-plane operations below are proxied; everything else is 403.
//
// Stdlib only — builds to a single static binary (CGO_ENABLED=0).
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- config ----

type Tenant struct {
	Key          string `json:"key"`
	ID           string `json:"id"`
	MaxSandboxes int    `json:"max_sandboxes"` // 0 = unlimited
	MaxCPUCount  int    `json:"max_cpu_count"` // 0 = no clamp
	MaxMemoryMB  int    `json:"max_memory_mb"` // 0 = no clamp
}

type tenantsFile struct {
	Tenants []Tenant `json:"tenants"`
}

// ---- usage ledger ----

type event struct {
	Type      string `json:"type"` // "open" | "close"
	Tenant    string `json:"tenant"`
	SandboxID string `json:"sandbox_id"`
	Template  string `json:"template,omitempty"`
	CPU       int    `json:"cpu,omitempty"`
	MemMB     int    `json:"mem_mb,omitempty"`
	TS        int64  `json:"ts"` // unix seconds
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

// ownerOf returns the tenant that owns an open sandbox. It is the authorization
// primitive: a sandbox the gateway never created (out-of-band / warm-pool
// replica) isn't in the ledger, so it has no owner and is invisible through the
// gateway. recordOpen runs before the create response is written, so ownership
// is consistent the instant the client learns the sandbox id.
func (l *Ledger) ownerOf(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	iv, ok := l.open[id]
	if !ok {
		return "", false
	}
	return iv.Tenant, true
}

// committed sums the currently-open sandboxes into fleet-wide committed load:
// vCPU (clamped to >=1 each), memory MB, and sandbox count. This is what's
// actually scheduled onto the worker fleet right now.
func (l *Ledger) committed() (vcpu, memMB, sandboxes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, iv := range l.open {
		vcpu += max1(iv.CPU)
		memMB += iv.MemMB
		sandboxes++
	}
	return
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

	// capacity charged to a sandbox when the request specifies no cpu/memory AND
	// the tenant has no clamp. Without it an unclamped sandbox books 0 MB and
	// under-reports committed load. Set to your template's real footprint.
	defaultCPU   int
	defaultMemMB int

	// fleet capacity (operator-supplied; total schedulable across worker nodes).
	// Drives the /admin/capacity "when do I add a node?" readout. As you add
	// workers, bump these. Portable: the same signal a Worker autoscaler reads.
	fleetNodes int
	fleetVCPU  int
	fleetMemMB int
	targetUtil float64 // headroom threshold, e.g. 0.80

	// recent create rejections (429s) for the scale signal.
	rmu     sync.Mutex
	rejects []int64 // unix-second timestamps, pruned to last 15m
}

// recordReject notes a 429 (quota/capacity rejection) for the scale signal.
func (g *Gateway) recordReject(now int64) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	g.rejects = append(g.rejects, now)
	// prune anything older than 15m
	cut := now - 15*60
	i := 0
	for i < len(g.rejects) && g.rejects[i] < cut {
		i++
	}
	g.rejects = g.rejects[i:]
}

// rejectStats returns rejection counts in the last 5m / 15m.
func (g *Gateway) rejectStats(now int64) (last5m, last15m int) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	for _, ts := range g.rejects {
		if ts >= now-15*60 {
			last15m++
			if ts >= now-5*60 {
				last5m++
			}
		}
	}
	return
}

func (g *Gateway) tenantFor(r *http.Request) (Tenant, bool) {
	// http.Header.Get is case-insensitive, so X-API-Key covers X-API-KEY too.
	key := r.Header.Get("X-API-Key")
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

// statusRecorder captures the status code of a proxied response so the caller
// can decide whether to settle meters (only on a confirmed terminal state).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// parseSandboxPath matches /sandboxes/{id} or /v2/sandboxes/{id} with an
// optional /subpath. Returns the id, whether a subpath follows, and whether it
// matched at all. The bare list paths (/sandboxes, /v2/sandboxes) do not match.
func parseSandboxPath(p string) (id string, subpath, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(p, "/sandboxes/"):
		rest = strings.TrimPrefix(p, "/sandboxes/")
	case strings.HasPrefix(p, "/v2/sandboxes/"):
		rest = strings.TrimPrefix(p, "/v2/sandboxes/")
	default:
		return "", false, false
	}
	if rest == "" {
		return "", false, false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], true, true
	}
	return rest, false, true
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

	// cube-api serves every route at BOTH "/" and "/cubeapi/v1/". Normalize the
	// prefix for routing/authorization decisions only — the proxy still forwards
	// the ORIGINAL path, so cube-api receives whichever form the client used.
	p := r.URL.Path
	if p == "/cubeapi/v1" || p == "/cubeapi/v1/" {
		p = "/"
	} else if strings.HasPrefix(p, "/cubeapi/v1/") {
		p = strings.TrimPrefix(p, "/cubeapi/v1")
	}

	switch {
	case p == "/health":
		g.proxy.ServeHTTP(w, r) // upstream liveness
		return
	case r.Method == http.MethodPost && p == "/sandboxes":
		g.handleCreate(w, r, t)
		return
	case r.Method == http.MethodGet && (p == "/sandboxes" || p == "/v2/sandboxes"):
		g.handleList(w, r, t)
		return
	case r.Method == http.MethodGet && (p == "/templates" || strings.HasPrefix(p, "/templates/")):
		g.proxy.ServeHTTP(w, r) // read-only template info, needed to launch
		return
	}

	// Any specific sandbox (and its subpaths: logs, timeout, pause, resume,
	// connect, snapshots, rollback) — enforce ownership before touching the box.
	if id, subpath, ok := parseSandboxPath(p); ok {
		owner, known := g.ledger.ownerOf(id)
		if !known || owner != t.ID {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if r.Method == http.MethodDelete && !subpath {
			g.handleDelete(w, r, id)
			return
		}
		g.proxy.ServeHTTP(w, r)
		return
	}

	// DEFAULT-DENY. cube-api has no tenant model, so everything else is a shared
	// or control-plane surface a tenant must not reach through this gateway:
	// template mutation (POST/PATCH/DELETE /templates*), /snapshots, /cluster/*,
	// /nodes*, /config, /store/*, /agenthub/*, and any unknown path.
	writeErr(w, http.StatusForbidden, "forbidden: not a tenant-scoped operation")
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
		g.recordReject(time.Now().Unix())
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("tenant %s sandbox quota reached (%d)", t.ID, t.MaxSandboxes))
		return
	}

	// Resource clamps, then a default floor so an unclamped/unsized sandbox can
	// never book 0 capacity (which would under-report committed load).
	cpu := asInt(m["cpuCount"])
	if t.MaxCPUCount > 0 && (cpu == 0 || cpu > t.MaxCPUCount) {
		cpu = t.MaxCPUCount
	}
	if cpu <= 0 {
		cpu = max1(g.defaultCPU)
	}
	m["cpuCount"] = cpu

	mem := asInt(m["memoryMB"])
	if t.MaxMemoryMB > 0 && (mem == 0 || mem > t.MaxMemoryMB) {
		mem = t.MaxMemoryMB
	}
	if mem <= 0 {
		mem = g.defaultMemMB
		if mem < 1 {
			mem = 1
		}
	}
	m["memoryMB"] = mem

	// Tag tenant in metadata (overwrite any client-supplied value — this is the
	// basis for ownership and the tenant-scoped list).
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

// handleDelete proxies the destroy to the box, then settles the meter ONLY when
// the box confirms removal — 2xx, or 404/410 meaning it was already gone
// (reconciliation). On a 5xx/transient failure capacity is NOT freed, since the
// sandbox may still be running. Ownership is already checked by the caller.
func (g *Gateway) handleDelete(w http.ResponseWriter, r *http.Request, id string) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	g.proxy.ServeHTTP(rec, r)
	if rec.status < 300 || rec.status == http.StatusNotFound || rec.status == http.StatusGone {
		g.ledger.recordClose(id, time.Now().Unix())
	}
}

// handleList proxies the list (preserving the caller's path + query so /v2 and
// /cubeapi/v1 shapes work) and returns ONLY the sandboxes this tenant owns. The
// tenant post-filter is authoritative, so a crafted metadata query can't widen
// the result set. If the body can't be parsed as an array we return [] rather
// than risk leaking another tenant's sandboxes.
func (g *Gateway) handleList(w http.ResponseWriter, r *http.Request, t Tenant) {
	req, _ := http.NewRequest(http.MethodGet, g.upstream.String()+r.URL.Path, nil)
	req.URL.RawQuery = r.URL.RawQuery
	copyHeaders(req.Header, r.Header)

	resp, err := g.client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	var list []map[string]any
	w.Header().Set("Content-Type", "application/json")
	if json.Unmarshal(body, &list) != nil {
		w.Write([]byte("[]"))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		if meta, ok := s["metadata"].(map[string]any); ok {
			if tn, _ := meta["tenant"].(string); tn == t.ID {
				out = append(out, s)
			}
		}
	}
	b, _ := json.Marshal(out)
	w.Write(b)
}

func (g *Gateway) handleAdmin(w http.ResponseWriter, r *http.Request) {
	// Constant-time compare so the admin token isn't a timing oracle.
	if g.admin == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(g.admin)) != 1 {
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
	case "/admin/capacity":
		g.writeCapacity(w)
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

// writeCapacity is the "when do I add a worker node?" readout: live committed
// load vs operator-declared fleet capacity, plus the recent 429 rate, plus a
// recommendation. Stateless and portable — the same signal a future edge/Worker
// autoscaler would consume.
func (g *Gateway) writeCapacity(w http.ResponseWriter) {
	now := time.Now().Unix()
	vcpu, memMB, sandboxes := g.ledger.committed()
	r5, r15 := g.rejectStats(now)

	var utilVCPU, utilMem float64
	if g.fleetVCPU > 0 {
		utilVCPU = float64(vcpu) / float64(g.fleetVCPU)
	}
	if g.fleetMemMB > 0 {
		utilMem = float64(memMB) / float64(g.fleetMemMB)
	}
	util := utilVCPU
	if utilMem > util {
		util = utilMem // bottleneck dimension drives the decision
	}

	// recommend adding a node if we're over the target utilization OR we've
	// rejected creates recently (demand already exceeding capacity).
	add := false
	reason := fmt.Sprintf("util %.0f%% < target %.0f%%, no recent rejections",
		util*100, g.targetUtil*100)
	switch {
	case g.fleetVCPU == 0:
		reason = "fleet capacity not configured (set GATEWAY_FLEET_VCPU / GATEWAY_FLEET_MEM_MB)"
	case r5 > 0:
		add = true
		reason = fmt.Sprintf("%d create(s) rejected in last 5m — capacity exceeded", r5)
	case util >= g.targetUtil:
		add = true
		reason = fmt.Sprintf("util %.0f%% >= target %.0f%% — running hot", util*100, g.targetUtil*100)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"as_of": time.Now().UTC().Format(time.RFC3339),
		"fleet": map[string]any{
			"nodes": g.fleetNodes, "vcpu": g.fleetVCPU, "mem_mb": g.fleetMemMB,
			"target_util": g.targetUtil,
		},
		"committed": map[string]any{
			"vcpu": vcpu, "mem_mb": memMB, "sandboxes": sandboxes,
		},
		"headroom": map[string]any{
			"vcpu": g.fleetVCPU - vcpu, "mem_mb": g.fleetMemMB - memMB,
			"utilization": util, "util_vcpu": utilVCPU, "util_mem": utilMem,
		},
		"rejections": map[string]any{"last_5m": r5, "last_15m": r15},
		"scale":      map[string]any{"add_node": add, "reason": reason},
	})
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

// copyHeaders forwards request headers to the upstream hop but ALWAYS strips the
// caller's inbound credentials, so a tenant key never reaches cube-api.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Content-Length") ||
			strings.EqualFold(k, "Authorization") ||
			strings.EqualFold(k, "X-API-Key") {
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
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

func main() {
	addr := flag.String("addr", env("GATEWAY_ADDR", ":8088"), "listen address")
	up := flag.String("upstream", env("CUBE_UPSTREAM", "http://127.0.0.1:3000"), "cube-api upstream")
	tenantsPath := flag.String("tenants", env("GATEWAY_TENANTS", "tenants.json"), "tenants config file")
	usagePath := flag.String("usage", env("GATEWAY_USAGE_LOG", "usage.jsonl"), "usage ledger (JSONL)")
	admin := flag.String("admin-token", env("GATEWAY_ADMIN_TOKEN", ""), "admin API token")
	defaultCPU := flag.Int("default-cpu", envInt("GATEWAY_DEFAULT_CPU", 1), "vCPU charged to an unclamped, unsized sandbox")
	defaultMemMB := flag.Int("default-mem-mb", envInt("GATEWAY_DEFAULT_MEM_MB", 512), "memory MB charged to an unclamped, unsized sandbox")
	fleetNodes := flag.Int("fleet-nodes", envInt("GATEWAY_FLEET_NODES", 1), "worker nodes in the fleet")
	fleetVCPU := flag.Int("fleet-vcpu", envInt("GATEWAY_FLEET_VCPU", 0), "total schedulable vCPU across the fleet (0=unset)")
	fleetMemMB := flag.Int("fleet-mem-mb", envInt("GATEWAY_FLEET_MEM_MB", 0), "total schedulable memory MB across the fleet (0=unset)")
	targetUtil := flag.Float64("target-util", envFloat("GATEWAY_TARGET_UTIL", 0.80), "utilization at which to recommend adding a node")
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

	// The transparent proxy must also strip inbound tenant credentials so they
	// never reach cube-api on the proxied (ownership-gated) paths.
	rp := httputil.NewSingleHostReverseProxy(upURL)
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		baseDirector(req)
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}

	g := &Gateway{
		tenants:      tenants,
		byID:         byID,
		upstream:     upURL,
		proxy:        rp,
		ledger:       ledger,
		admin:        *admin,
		client:       &http.Client{Timeout: 60 * time.Second},
		defaultCPU:   *defaultCPU,
		defaultMemMB: *defaultMemMB,
		fleetNodes:   *fleetNodes,
		fleetVCPU:    *fleetVCPU,
		fleetMemMB:   *fleetMemMB,
		targetUtil:   *targetUtil,
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

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return d
}
