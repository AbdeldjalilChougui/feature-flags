// Package sdk is the Go client for the flag server. It keeps every flag of
// its environment in a local cache, kept fresh over Server-Sent Events, and
// evaluates flags locally: no network call per evaluation, and the last
// known configuration keeps working if the server goes away.
//
//	c := sdk.New("https://flags.example.com", os.Getenv("FLAGS_SDK_KEY"))
//	go c.Start(ctx)
//	c.WaitReady(ctx)
//	if c.Bool("new-checkout", flag.Context{"key": userID, "country": "DZ"}, false) { … }
package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AbdeldjalilChougui/feature-flags/flag"
)

// Payload mirrors the server's /sdk/flags response.
type Payload struct {
	Version     int64                   `json:"version"`
	Environment string                  `json:"environment"`
	Flags       map[string]flag.EnvFlag `json:"flags"`
}

type Client struct {
	baseURL string
	sdkKey  string
	http    *http.Client // for one-shot requests (with timeout)
	stream  *http.Client // for the long-lived stream (no timeout)
	log     *slog.Logger
	// IdleTimeout: reconnect if the stream is silent this long (the server
	// pings every 20s by default).
	idle       time.Duration
	maxBackoff time.Duration

	mu      sync.RWMutex
	payload *Payload

	readyOnce sync.Once
	ready     chan struct{}
	onUpdate  func(version int64)
}

// Option configures a Client.
type Option func(*Client)

func WithLogger(l *slog.Logger) Option          { return func(c *Client) { c.log = l } }
func WithIdleTimeout(d time.Duration) Option    { return func(c *Client) { c.idle = d } }
func WithMaxBackoff(d time.Duration) Option     { return func(c *Client) { c.maxBackoff = d } }
func WithOnUpdate(f func(version int64)) Option { return func(c *Client) { c.onUpdate = f } }

func New(baseURL, sdkKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"), sdkKey: sdkKey,
		http: &http.Client{Timeout: 10 * time.Second}, stream: &http.Client{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), idle: 60 * time.Second, maxBackoff: 30 * time.Second,
		ready: make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Start keeps the cache up to date until ctx is cancelled: it streams
// changes, and when streaming fails it polls once and retries with
// exponential backoff (so a proxy that breaks SSE still gets updates).
func (c *Client) Start(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		connected, err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = 250 * time.Millisecond
		}
		c.log.Warn("flag stream interrupted", "err", err)
		if perr := c.Refresh(ctx); perr != nil {
			c.log.Warn("flag poll failed", "err", perr)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.maxBackoff)
	}
}

// WaitReady blocks until the first configuration has arrived.
func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Refresh fetches the configuration once with a plain GET.
func (c *Client) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sdk/flags", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.sdkKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /sdk/flags: HTTP %d", resp.StatusCode)
	}
	var p Payload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return err
	}
	c.apply(&p)
	return nil
}

// apply installs p unless we already have something newer.
func (c *Client) apply(p *Payload) {
	c.mu.Lock()
	if c.payload != nil && p.Version < c.payload.Version {
		c.mu.Unlock()
		return
	}
	c.payload = p
	c.mu.Unlock()
	c.readyOnce.Do(func() { close(c.ready) })
	if c.onUpdate != nil {
		c.onUpdate(p.Version)
	}
}

// streamOnce runs one SSE connection. connected reports whether at least
// one event was received.
func (c *Client) streamOnce(ctx context.Context) (connected bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sdk/stream", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.sdkKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /sdk/stream: HTTP %d", resp.StatusCode)
	}
	// Watchdog: any line (including ": ping") resets it.
	watchdog := time.AfterFunc(c.idle, cancel)
	defer watchdog.Stop()

	r := bufio.NewReaderSize(resp.Body, 64<<10)
	var event string
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				return connected, errors.New("stream idle for too long")
			}
			return connected, err
		}
		watchdog.Reset(c.idle)
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "": // dispatch
			if event == "put" && data.Len() > 0 {
				var p Payload
				if err := json.Unmarshal([]byte(data.String()), &p); err != nil {
					return connected, fmt.Errorf("bad payload: %w", err)
				}
				c.apply(&p)
				connected = true
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"): // comment / keepalive
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

// Version is the configuration version in the cache (0 before the first).
func (c *Client) Version() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.payload == nil {
		return 0
	}
	return c.payload.Version
}

// Evaluate returns the full result. ok is false when the flag is unknown
// or nothing has been received yet.
func (c *Client) Evaluate(key string, ctx flag.Context) (flag.Result, bool) {
	c.mu.RLock()
	p := c.payload
	c.mu.RUnlock()
	if p == nil {
		return flag.Result{}, false
	}
	f, ok := p.Flags[key]
	if !ok {
		return flag.Result{}, false
	}
	return flag.Evaluate(f, ctx), true
}

// Bool evaluates a boolean flag, returning def if it's unknown or not a bool.
func (c *Client) Bool(key string, ctx flag.Context, def bool) bool {
	if r, ok := c.Evaluate(key, ctx); ok {
		if b, ok := r.Value.(bool); ok {
			return b
		}
	}
	return def
}

// String evaluates a string flag.
func (c *Client) String(key string, ctx flag.Context, def string) string {
	if r, ok := c.Evaluate(key, ctx); ok {
		if s, ok := r.Value.(string); ok {
			return s
		}
	}
	return def
}

// Float evaluates a numeric flag.
func (c *Client) Float(key string, ctx flag.Context, def float64) float64 {
	if r, ok := c.Evaluate(key, ctx); ok {
		if f, ok := r.Value.(float64); ok {
			return f
		}
	}
	return def
}
