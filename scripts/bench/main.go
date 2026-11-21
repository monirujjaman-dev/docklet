// Command bench measures Docklet against the Global Budgets of the
// development plan and, optionally, the idle memory of other containers
// (e.g. a Dokploy install) for comparison.
//
//	go build -o bin/docklet ./cmd/docklet
//	go run ./scripts/bench -binary bin/docklet                      # local checks
//	sudo go run ./scripts/bench -binary bin/docklet \
//	    -containers docklet,docklet-caddy                           # + installed RSS
//	sudo go run ./scripts/bench -binary bin/docklet -containers docklet,docklet-caddy \
//	    -compare 'Dokploy=dokploy,dokploy-postgres,dokploy-redis,dokploy-traefik'
//
// It starts its own Docklet on a free port with a throwaway data directory
// (Docker and Caddy integration off) for cold start, idle RSS, bundle size
// and API latency. Container RSS is read from /proc/<pid>/status of each
// container's processes, so it needs to run on the Docker host, as root.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type result struct {
	name   string
	value  string
	limit  string
	pass   *bool
	detail string
}

func check(ok bool) *bool { return &ok }

func main() {
	binary := flag.String("binary", "bin/docklet", "Docklet binary to measure")
	containers := flag.String("containers", "", "installed Docklet containers to measure, e.g. docklet,docklet-caddy")
	compare := flag.String("compare", "", "other platforms: Name=container1,container2[;Name2=…]")
	requests := flag.Int("requests", 400, "requests per API endpoint")
	idle := flag.Duration("idle", 20*time.Second, "how long the spawned instance idles before measuring RSS")
	jsonOut := flag.Bool("json", false, "print JSON instead of a Markdown table")
	flag.Parse()

	var res []result
	add := func(r result) { res = append(res, r) }

	// Binary size.
	if fi, err := os.Stat(*binary); err == nil {
		mb := float64(fi.Size()) / (1 << 20)
		add(result{name: "Binary size", value: fmt.Sprintf("%.1f MB", mb), limit: "≤ 40 MB", pass: check(mb <= 40)})
	} else {
		fatal("binary: %v", err)
	}

	// Cold start: exec → /api/health answers.
	inst, cold := startInstance(*binary)
	defer inst.stop()
	add(result{name: "Cold start (serve → healthy)", value: fmt.Sprintf("%d ms", cold.Milliseconds()), limit: "≤ 200 ms", pass: check(cold <= 200*time.Millisecond)})

	// API latency on non-build endpoints, after creating the admin.
	c := inst.login()
	for i := 0; i < 20; i++ {
		c.post("/api/projects", fmt.Sprintf(`{"name":"bench-%d"}`, i))
	}
	var all []time.Duration
	for _, ep := range []string{"/api/auth/me", "/api/projects", "/api/audit?limit=50", "/api/settings/panel"} {
		d := c.latencies(ep, *requests, 4)
		all = append(all, d...)
		add(result{name: "API p95 " + ep, value: ms(pct(d, 95)), limit: "≤ 50 ms", pass: check(pct(d, 95) <= 50*time.Millisecond),
			detail: fmt.Sprintf("p50 %s, p99 %s, %d req, 4 concurrent", ms(pct(d, 50)), ms(pct(d, 99)), len(d))})
	}
	add(result{name: "API p95 (all endpoints)", value: ms(pct(all, 95)), limit: "≤ 50 ms", pass: check(pct(all, 95) <= 50*time.Millisecond)})

	// Dashboard bundle: gzipped size of the initial JS.
	js := inst.bundleGzip()
	add(result{name: "Dashboard JS (gzip, initial)", value: fmt.Sprintf("%.1f KB", float64(js)/1024), limit: "≤ 150 KB", pass: check(js <= 150<<10)})

	// Idle RSS of the spawned instance (no Docker/Caddy work).
	time.Sleep(*idle)
	rss := rssOf(inst.cmd.Process.Pid)
	add(result{name: "Docklet idle RSS (bench instance)", value: mbs(rss), limit: "≤ 40 MB", pass: check(rss <= 40<<20),
		detail: "Docker and Caddy integration off"})

	// Installed containers.
	if *containers != "" {
		total, parts := containerRSS(strings.Split(*containers, ","))
		names := strings.Split(*containers, ",")
		if len(names) > 0 {
			if v, ok := parts[names[0]]; ok {
				add(result{name: "Docklet idle RSS (installed)", value: mbs(v), limit: "≤ 40 MB", pass: check(v <= 40<<20), detail: names[0]})
			}
		}
		add(result{name: "Docklet + Caddy idle RSS (installed)", value: mbs(total), limit: "≤ 80 MB", pass: check(total <= 80<<20), detail: describe(parts)})
	}
	for _, spec := range strings.Split(*compare, ";") {
		name, list, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		total, parts := containerRSS(strings.Split(list, ","))
		add(result{name: name + " idle RSS (for comparison)", value: mbs(total), detail: describe(parts)})
	}

	if *jsonOut {
		out := []map[string]any{}
		for _, r := range res {
			m := map[string]any{"name": r.name, "value": r.value, "limit": r.limit, "detail": r.detail}
			if r.pass != nil {
				m["pass"] = *r.pass
			}
			out = append(out, m)
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Println("| Metric | Value | Budget | Result | Notes |")
		fmt.Println("|---|---|---|---|---|")
		for _, r := range res {
			verdict := ""
			if r.pass != nil {
				verdict = "✅ pass"
				if !*r.pass {
					verdict = "❌ FAIL"
				}
			}
			fmt.Printf("| %s | %s | %s | %s | %s |\n", r.name, r.value, r.limit, verdict, r.detail)
		}
	}
	for _, r := range res {
		if r.pass != nil && !*r.pass {
			inst.stop()
			os.Exit(1)
		}
	}
}

var current *instance // stopped by fatal

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", args...)
	if current != nil {
		current.stop()
	}
	os.Exit(2)
}

type instance struct {
	cmd  *exec.Cmd
	base string
	dir  string
	once sync.Once
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func startInstance(binary string) (*instance, time.Duration) {
	dir, err := os.MkdirTemp("", "docklet-bench-")
	if err != nil {
		fatal("%v", err)
	}
	port := freePort()
	inst := &instance{dir: dir, base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	current = inst
	inst.cmd = exec.Command(binary, "serve", "-addr", fmt.Sprintf("127.0.0.1:%d", port), "-data-dir", dir, //nolint:gosec // benchmark tool
		"-caddy-admin", "off", "-docker-socket", "off", "-no-update-check", "-log-level", "warn")
	inst.cmd.Stderr = os.Stderr
	start := time.Now()
	if err := inst.cmd.Start(); err != nil {
		fatal("start %s: %v", binary, err)
	}
	hc := &http.Client{Timeout: time.Second}
	for time.Since(start) < 10*time.Second {
		if res, err := hc.Get(inst.base + "/api/health"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 {
				return inst, time.Since(start)
			}
		}
		time.Sleep(time.Millisecond)
	}
	inst.stop()
	fatal("docklet did not become healthy")
	return nil, 0
}

func (i *instance) stop() {
	i.once.Do(func() {
		if i.cmd.Process != nil {
			_ = i.cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = i.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = i.cmd.Process.Kill()
			}
		}
		_ = os.RemoveAll(i.dir)
	})
}

type client struct {
	hc   *http.Client
	base string
	csrf string
}

func (i *instance) login() *client {
	jar, _ := cookiejar.New(nil)
	c := &client{hc: &http.Client{Jar: jar, Timeout: 10 * time.Second}, base: i.base}
	pw := "bench-password-1234"
	c.post("/api/setup", `{"username":"admin","password":"`+pw+`"}`)
	body := c.post("/api/auth/login", `{"username":"admin","password":"`+pw+`"}`)
	var me struct {
		CsrfToken string `json:"csrfToken"`
	}
	_ = json.Unmarshal(body, &me)
	if me.CsrfToken == "" {
		res, err := c.hc.Get(c.base + "/api/auth/me")
		if err == nil {
			_ = json.NewDecoder(res.Body).Decode(&me)
			_ = res.Body.Close()
		}
	}
	c.csrf = me.CsrfToken
	return c
}

func (c *client) post(path, body string) []byte {
	req, _ := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		fatal("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return b
}

func (c *client) latencies(path string, n, workers int) []time.Duration {
	var mu sync.Mutex
	out := make([]time.Duration, 0, n)
	jobs := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				t := time.Now()
				res, err := c.hc.Get(c.base + path)
				if err != nil {
					fatal("GET %s: %v", path, err)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if res.StatusCode != 200 {
					fatal("GET %s: %s", path, res.Status)
				}
				d := time.Since(t)
				mu.Lock()
				out = append(out, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func pct(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (len(s)*p + 99) / 100
	if i < 1 {
		i = 1
	}
	return s[i-1]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }
func mbs(b int64) string        { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }

var scriptSrc = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)

func (i *instance) bundleGzip() int {
	get := func(p string) []byte {
		res, err := http.Get(i.base + p) //nolint:gosec // local instance
		if err != nil {
			fatal("GET %s: %v", p, err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return b
	}
	total := 0
	for _, m := range scriptSrc.FindAllSubmatch(get("/"), -1) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(get(string(m[1])))
		_ = zw.Close()
		total += buf.Len()
	}
	return total
}

// rssOf returns VmRSS of pid and its descendants.
func rssOf(pid int) int64 {
	var total int64
	for _, p := range append([]int{pid}, descendants(pid)...) {
		total += vmRSS(p)
	}
	return total
}

func vmRSS(pid int) int64 {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func descendants(pid int) []int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if c, err := strconv.Atoi(f); err == nil {
			out = append(out, c)
			out = append(out, descendants(c)...)
		}
	}
	return out
}

// containerRSS sums VmRSS of every process in each container (docker top).
func containerRSS(names []string) (int64, map[string]int64) {
	parts := map[string]int64{}
	var total int64
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		out, err := exec.Command("docker", "top", n, "-eo", "pid").Output() //nolint:gosec // benchmark tool
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench: docker top %s: %v\n", n, err)
			continue
		}
		var sum int64
		for _, line := range strings.Split(string(out), "\n")[1:] {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
				sum += vmRSS(pid)
			}
		}
		parts[n] = sum
		total += sum
	}
	return total, parts
}

func describe(parts map[string]int64) string {
	keys := make([]string, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", k, mbs(parts[k]))
	}
	return b.String()
}
