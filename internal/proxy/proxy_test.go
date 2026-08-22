package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// singleAccountProxy returns a proxy handler backed by a one-account pool
// pointing at upstreamURL.
func singleAccountProxy(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstream = upstreamURL
	cfg.Pool.ExhaustedMode = "fail" // tests fail fast; hold mode has its own suite
	p := pool.New([]*pool.Account{pool.NewAccount("test", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func TestRequestFidelity(t *testing.T) {
	type seen struct {
		method, path, query, body string
		headers                   http.Header
	}
	got := make(chan seen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	front := httptest.NewServer(singleAccountProxy(t, upstream.URL))
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages?stream=true&foo=bar",
		strings.NewReader(`{"model":"claude","max_tokens":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")
	req.Header.Set("User-Agent", "claude-cli/2.1.44")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	s := <-got
	if s.method != http.MethodPost {
		t.Errorf("method = %q", s.method)
	}
	if s.path != "/v1/messages" {
		t.Errorf("path = %q", s.path)
	}
	if s.query != "stream=true&foo=bar" {
		t.Errorf("query = %q", s.query)
	}
	if s.body != `{"model":"claude","max_tokens":1}` {
		t.Errorf("body = %q", s.body)
	}
	for _, h := range []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta", "User-Agent"} {
		if s.headers.Get(h) != req.Header.Get(h) {
			t.Errorf("header %s = %q, want %q", h, s.headers.Get(h), req.Header.Get(h))
		}
	}
}

func TestResponseFidelity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Remaining", "4999")
		w.Header().Set("X-Request-Id", "req_abc123")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	front := httptest.NewServer(singleAccountProxy(t, upstream.URL))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Anthropic-Ratelimit-Remaining"); got != "4999" {
		t.Errorf("ratelimit header = %q", got)
	}
	if got := resp.Header.Get("X-Request-Id"); got != "req_abc123" {
		t.Errorf("x-request-id = %q", got)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

func TestStreamingIsIncremental(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: chunk-1\n\n")
		fl.Flush()
		// Hold the response open; the client must already have chunk-1.
		<-release
		fmt.Fprint(w, "data: chunk-2\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	front := httptest.NewServer(singleAccountProxy(t, upstream.URL))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			errc <- err
			return
		}
		first <- line
	}()

	select {
	case line := <-first:
		if !strings.Contains(line, "chunk-1") {
			t.Errorf("first line = %q, want chunk-1", line)
		}
	case err := <-errc:
		t.Fatalf("read first chunk: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk not received before upstream finished: response was buffered")
	}

	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "chunk-2") {
		t.Errorf("rest = %q, want chunk-2", rest)
	}
}

func TestHostHeaderSetToUpstream(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Host
	}))
	defer upstream.Close()

	front := httptest.NewServer(singleAccountProxy(t, upstream.URL))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	req.Host = "api.anthropic.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	if h := <-got; h != upstreamHost {
		t.Errorf("upstream saw Host = %q, want %q", h, upstreamHost)
	}
}

func TestAuthInjection(t *testing.T) {
	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	}))
	defer upstream.Close()

	front := httptest.NewServer(singleAccountProxy(t, upstream.URL))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", "client-key-should-be-dropped")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.44")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	hdr := <-got
	if a := hdr.Get("Authorization"); a != "Bearer tok" {
		t.Errorf("Authorization = %q", a)
	}
	if k := hdr.Get("X-Api-Key"); k != "" {
		t.Errorf("x-api-key not dropped: %q", k)
	}
	if b := hdr.Get("Anthropic-Beta"); b != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta mutated: %q", b)
	}
	if ua := hdr.Get("User-Agent"); ua != "claude-cli/2.1.44" {
		t.Errorf("user-agent mutated: %q", ua)
	}
}
