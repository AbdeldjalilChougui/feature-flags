package sdk_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AbdeldjalilChougui/feature-flags/flag"
	"github.com/AbdeldjalilChougui/feature-flags/sdk"
	"github.com/AbdeldjalilChougui/feature-flags/server"
)

const admin = "adm1n"

func adminCall(t *testing.T, base, method, path, body string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func start(t *testing.T) (*server.Store, *httptest.Server, string) {
	st, err := server.Open(filepath.Join(t.TempDir(), "f.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, server.Config{AdminToken: admin, KeepAlive: 100 * time.Millisecond}))
	t.Cleanup(srv.Close)
	var key string
	for _, e := range st.Environments() {
		if e.Key == "production" {
			key = e.SDKKey
		}
	}
	adminCall(t, srv.URL, "POST", "/api/flags", `{"key":"new-checkout","variants":{"on":true,"off":false},
	  "environments":{"production":{"enabled":true,"off_variant":"off","default":{"variant":"off"},
	  "rules":[{"clauses":[{"attribute":"plan","op":"in","values":["pro","team"]}],"serve":{"variant":"on"}}]}}}`)
	adminCall(t, srv.URL, "POST", "/api/flags", `{"key":"banner","variants":{"a":"blue","b":"green"}}`)
	return st, srv, key
}

func TestStreamingClient(t *testing.T) {
	_, srv, key := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := sdk.New(srv.URL, key, sdk.WithIdleTimeout(time.Second))
	go c.Start(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
	defer wcancel()
	if err := c.WaitReady(wctx); err != nil {
		t.Fatal(err)
	}
	pro := flag.Context{"key": "u1", "plan": "pro"}
	free := flag.Context{"key": "u2", "plan": "free"}
	if !c.Bool("new-checkout", pro, false) || c.Bool("new-checkout", free, true) {
		t.Fatal("rules not applied locally")
	}
	// banner is off in production: it serves its off variant ("a" = "blue").
	if c.String("banner", pro, "fallback") != "blue" || c.Float("banner", pro, 7) != 7 {
		t.Fatal("off variant / type mismatch fallback")
	}
	if c.Bool("unknown", pro, true) != true {
		t.Fatal("unknown flags return the default")
	}
	// Turn the flag off: the change is pushed to the SDK.
	v := c.Version()
	adminCall(t, srv.URL, "POST", "/api/flags/new-checkout/environments/production/toggle", "")
	eventually(t, "toggle to reach the SDK", func() bool { return c.Version() > v && !c.Bool("new-checkout", pro, true) })

	r, ok := c.Evaluate("new-checkout", pro)
	if !ok || r.Reason.Kind != "OFF" {
		t.Fatalf("%+v", r)
	}
	// The server goes away: the cache keeps answering.
	srv.CloseClientConnections()
	srv.Close()
	time.Sleep(50 * time.Millisecond)
	if r, ok := c.Evaluate("new-checkout", pro); !ok || r.Variant != "off" {
		t.Fatal("cache lost after the server went down")
	}
}

func TestPollingFallback(t *testing.T) {
	_, srv, key := start(t)
	// A proxy in front that breaks SSE but lets normal requests through.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sdk/stream" {
			http.Error(w, "streaming not allowed", http.StatusBadGateway)
			return
		}
		req, _ := http.NewRequest(r.Method, srv.URL+r.URL.RequestURI(), r.Body)
		req.Header = r.Header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			w.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}))
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := sdk.New(proxy.URL, key, sdk.WithMaxBackoff(50*time.Millisecond))
	go c.Start(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
	defer wcancel()
	if err := c.WaitReady(wctx); err != nil {
		t.Fatal("polling fallback never delivered a configuration")
	}
	v := c.Version()
	adminCall(t, srv.URL, "POST", "/api/flags/banner/environments/production/toggle", "")
	eventually(t, "polled update", func() bool { return c.Version() > v })
	if r, _ := c.Evaluate("banner", flag.Context{"key": "x"}); r.Reason.Kind != "DEFAULT" {
		t.Fatalf("banner should now be on and serve its default: %+v", r)
	}
}

func TestBadKey(t *testing.T) {
	_, srv, _ := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := sdk.New(srv.URL, "wrong", sdk.WithMaxBackoff(20*time.Millisecond))
	go c.Start(ctx)
	if err := c.WaitReady(ctx); err == nil {
		t.Fatal("ready with a bad key")
	}
}
