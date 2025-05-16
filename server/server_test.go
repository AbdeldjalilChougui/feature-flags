package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const admin = "adm1n"

func setup(t *testing.T) (*Store, *httptest.Server) {
	st, err := Open(filepath.Join(t.TempDir(), "flags.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Config{AdminToken: admin, KeepAlive: 50 * time.Millisecond}))
	t.Cleanup(srv.Close)
	return st, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	bufio.NewReader(resp.Body).WriteTo(&sb)
	return resp.StatusCode, sb.String()
}

func sdkKey(st *Store, env string) string {
	for _, e := range st.Environments() {
		if e.Key == env {
			return e.SDKKey
		}
	}
	return ""
}

const bannerFlag = `{"key":"banner","description":"homepage banner","variants":{"blue":"#00f","green":"#0f0"},
 "environments":{"production":{"enabled":true,"off_variant":"blue","default":{"variant":"blue"},
   "rules":[{"clauses":[{"attribute":"country","op":"eq","values":["DZ"]}],"serve":{"variant":"green"}}]}}}`

func TestFlagCRUDAndValidation(t *testing.T) {
	st, srv := setup(t)
	if code, body := call(t, srv, "POST", "/api/flags", bannerFlag); code != 201 || !strings.Contains(body, `"salt"`) {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := call(t, srv, "POST", "/api/flags", bannerFlag); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	// Unconfigured environments get a default "off" config.
	f, _ := st.Flag("banner")
	if dev := f.Environments["development"]; dev == nil || dev.Enabled {
		t.Fatalf("development config: %+v", dev)
	}
	for _, bad := range []string{
		`{"key":"x","variants":{}}`,
		`{"key":"x","variants":{"a":1},"environments":{"staging":{"off_variant":"a","default":{"variant":"a"}}}}`,
		`{"key":"x","variants":{"a":1},"environments":{"production":{"off_variant":"a","default":{"rollout":[{"variant":"a","weight":10}]}}}}`,
		`{"key":"x","variants":{"a":1},"bogus":true}`,
	} {
		if code, body := call(t, srv, "POST", "/api/flags", bad); code != 400 {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	// Toggle and per-environment config.
	code, body := call(t, srv, "POST", "/api/flags/banner/environments/development/toggle", "")
	if code != 200 || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("toggle: %d %s", code, body)
	}
	code, body = call(t, srv, "PUT", "/api/flags/banner/environments/development",
		`{"enabled":true,"off_variant":"blue","default":{"rollout":[{"variant":"green","weight":5000},{"variant":"blue","weight":5000}]}}`)
	if code != 200 {
		t.Fatalf("put env: %d %s", code, body)
	}
	code, body = call(t, srv, "POST", "/api/evaluate", `{"environment":"production","flag":"banner","context":{"key":"u","country":"DZ"}}`)
	if code != 200 || !strings.Contains(body, `"variant":"green"`) || !strings.Contains(body, `"RULE_MATCH"`) {
		t.Fatalf("evaluate: %d %s", code, body)
	}
	if code, _ := call(t, srv, "DELETE", "/api/flags/banner", ""); code != 204 {
		t.Fatal(code)
	}
	if code, _ := call(t, srv, "GET", "/api/flags/banner", ""); code != 404 {
		t.Fatal(code)
	}
	// Environments.
	code, body = call(t, srv, "POST", "/api/environments", `{"key":"staging","name":"Staging"}`)
	if code != 201 || !strings.Contains(body, `"sdk_key":"sdk-`) {
		t.Fatalf("env: %d %s", code, body)
	}
	// State survives a restart.
	st2, err := Open(st.path)
	if err != nil || len(st2.Environments()) != 3 || st2.Version() != st.Version() {
		t.Fatalf("reload: %v %d", err, len(st2.Environments()))
	}
}

func TestAuth(t *testing.T) {
	st, srv := setup(t)
	resp, _ := http.Get(srv.URL + "/api/flags")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("api without token: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/sdk/flags?sdk_key=wrong")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("sdk with bad key: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/sdk/flags?sdk_key=" + url.QueryEscape(sdkKey(st, "production")))
	var p Payload
	json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	if resp.StatusCode != 200 || p.Environment != "production" {
		t.Fatalf("sdk flags: %d %+v", resp.StatusCode, p)
	}
}

// readEvent reads one SSE "put" event, skipping keepalive comments.
func readEvent(t *testing.T, r *bufio.Reader) Payload {
	t.Helper()
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && event == "put":
			var p Payload
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
}

func TestSSEStream(t *testing.T) {
	st, srv := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/sdk/stream", nil)
	req.Header.Set("Authorization", "Bearer "+sdkKey(st, "production"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	first := readEvent(t, r)
	if len(first.Flags) != 0 {
		t.Fatalf("%+v", first)
	}
	call(t, srv, "POST", "/api/flags", bannerFlag)
	next := readEvent(t, r)
	if next.Version <= first.Version || next.Flags["banner"].Variants["green"] != "#0f0" {
		t.Fatalf("update not streamed: %+v", next)
	}
	// Keepalive comments arrive while idle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, _ := r.ReadString('\n')
		if strings.HasPrefix(line, ": ping") {
			return
		}
	}
	t.Fatal("no keepalive")
}

func TestHtmxUI(t *testing.T) {
	_, srv := setup(t)
	call(t, srv, "POST", "/api/flags", bannerFlag)
	code, body := call(t, srv, "GET", "/admin?env=production", "")
	if code != 200 || !strings.Contains(body, `hx-post="/admin/flags/banner/toggle?env=production"`) || !strings.Contains(body, "/static/htmx.min.js") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/static/htmx.min.js", ""); code != 200 {
		t.Fatal(code)
	}
	// Writes without HX-Request are refused (CSRF guard).
	if code, _ := call(t, srv, "POST", "/admin/flags/banner/toggle?env=production", ""); code != 403 {
		t.Fatalf("toggle without HX header: %d", code)
	}
	code, body = call(t, srv, "POST", "/admin/flags/banner/toggle?env=production", "", "HX-Request", "true")
	if code != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<tr id="row-banner">`) || strings.Contains(body, `class="switch on"`) {
		t.Fatalf("toggle partial: %d %s", code, body)
	}
	form := "key=dark-mode&description=Dark+theme"
	code, body = call(t, srv, "POST", "/admin/flags?env=production", form, "HX-Request", "true", "Content-Type", "application/x-www-form-urlencoded")
	if code != 200 || !strings.Contains(body, `<div id="flags">`) || !strings.Contains(body, "dark-mode") {
		t.Fatalf("create: %d %s", code, body)
	}
	bad := "config=" + url.QueryEscape(`{"enabled":true,"off_variant":"nope","default":{"variant":"on"}}`)
	_, body = call(t, srv, "POST", "/admin/flags/dark-mode/config?env=production", bad, "HX-Request", "true", "Content-Type", "application/x-www-form-urlencoded")
	if !strings.Contains(body, `class="err"`) || !strings.Contains(body, "off_variant") {
		t.Fatalf("validation notice: %s", body)
	}
	good := "config=" + url.QueryEscape(`{"enabled":true,"off_variant":"off","default":{"rollout":[{"variant":"on","weight":2500},{"variant":"off","weight":7500}]}}`)
	_, body = call(t, srv, "POST", "/admin/flags/dark-mode/config?env=production", good, "HX-Request", "true", "Content-Type", "application/x-www-form-urlencoded")
	if !strings.Contains(body, "Saved") {
		t.Fatalf("save: %s", body)
	}
	_, body = call(t, srv, "GET", "/admin?env=production", "")
	if !strings.Contains(body, "rollout: on 25% / off 75%") {
		t.Fatalf("summary missing: %s", body)
	}
	test := "context=" + url.QueryEscape(`{"key":"user-1"}`)
	_, body = call(t, srv, "POST", "/admin/flags/dark-mode/test?env=production", test, "HX-Request", "true", "Content-Type", "application/x-www-form-urlencoded")
	if !strings.Contains(body, "percentage rollout") {
		t.Fatalf("test: %s", body)
	}
	code, body = call(t, srv, "GET", "/admin/flags/dark-mode?env=production", "")
	if code != 200 || !strings.Contains(body, "&#34;weight&#34;: 2500") {
		t.Fatalf("edit page: %d", code)
	}
}
