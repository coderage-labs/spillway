// Package admin serves the loopback-only admin listener (design doc §5,
// §7): read-only JSON API, SSE event stream, and the embedded web UI. The
// admin port is a separate trust class from the proxy port — never mixed.
package admin

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/netaddr"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// DefaultAddr is the admin listener bind (loopback only, §5).
const DefaultAddr = "127.0.0.1:7657"

// sseHeartbeat is how often the event stream emits a keep-alive comment.
const sseHeartbeat = 25 * time.Second

// eventsPath is the SSE stream. Named because authorized() has to single it
// out: it is the only endpoint that may carry its token in the query string.
const eventsPath = "/api/events"

//go:embed static/index.html static/logo.svg
var staticFS embed.FS

// Server is the admin API handler.
type Server struct {
	pool   *pool.Pool
	log    *reqlog.Log
	broker *events.Broker
	token  string
	// requireToken is set when the listener is not loopback-only, or when the
	// user configured a token explicitly.
	requireToken bool
	// configPath and onSettings enable the settings endpoint. Unset in tests
	// that only exercise the read-only API.
	configPath string
	onSettings func(*config.Config)
	// caWarning, when set, reports whether issue #66's stale-CA warning
	// should be shown on /api/state — typically (*proxy.Handler).StaleCAWarning.
	// Not a constructor parameter, for the same reason EnableSettings isn't:
	// tests exercising the read-only API do not need MITM wired up at all.
	caWarning func() bool
	// hostCovered and refreshHosts wire issue #87's live account-add to the
	// proxy handler's MITM host-set bookkeeping — see EnableLiveMITM. Both
	// nil in tests that only exercise the pool side of account-add (no MITM
	// at all): a nil hostCovered means every upstream reports "not covered",
	// which the handler already treats as "flag it, don't strand it".
	hostCovered  func(host string) bool
	refreshHosts func()
}

// EnableLiveMITM wires issue #87's live account-add to the proxy handler's
// CONNECT host-set bookkeeping. covers reports whether host already has a
// pre-minted MITM leaf — typically (*proxy.Handler).MITMCovers — used to
// decide whether a freshly added account's upstream needs a restart before
// CONNECT-mode (system-proxy) traffic to it is pooled. refresh recomputes
// the CONNECT-termination host set after a pool.Add — typically
// (*proxy.Handler).RefreshAllowedHosts; normally a no-op, since every
// provider's default upstream is pre-minted at startup regardless of which
// accounts exist that day, but correct to call regardless. Not a
// constructor parameter for the same reason EnableSettings isn't: tests
// exercising only the pool side of account-add need no MITM wired up at
// all.
func (s *Server) EnableLiveMITM(covers func(host string) bool, refresh func()) {
	s.hostCovered = covers
	s.refreshHosts = refresh
}

// EnableSettings turns on the editable-config endpoint. apply is called with
// the new config after a successful write so the running daemon can pick the
// change up without a restart.
func (s *Server) EnableSettings(configPath string, apply func(*config.Config)) {
	s.configPath = configPath
	s.onSettings = apply
}

// SetCAWarning wires the source for issue #66's stale-CA warning into
// /api/state. f is read fresh on every /api/state request — never cached —
// so the warning appears and decays live as the underlying detector's state
// changes.
func (s *Server) SetCAWarning(f func() bool) {
	s.caWarning = f
}

// New builds the admin handler. An empty token means the listener is
// loopback-only and unauthenticated by design: on loopback the token defends
// only against processes running as ANOTHER user (they can reach the port but
// cannot read the 0600 token file), while costing a secret in the URL and a
// dead tab on every restart. Off-loopback it is mandatory — see RequireToken.
func New(p *pool.Pool, l *reqlog.Log, b *events.Broker, token string) *Server {
	return &Server{pool: p, log: l, broker: b, token: token, requireToken: token != ""}
}

// RequireToken forces authentication. With no token set this fails every
// request closed rather than open, so a misconfiguration cannot silently
// expose the dashboard.
func (s *Server) RequireToken() { s.requireToken = true }

// IsUnix reports whether addr names a unix socket rather than a host:port.
// The convention is a path or an explicit unix:// prefix.
func IsUnix(addr string) bool {
	return strings.HasPrefix(addr, "unix://") ||
		strings.HasPrefix(addr, "/") || strings.HasPrefix(addr, "./") ||
		strings.HasPrefix(addr, "~/")
}

// SocketPath resolves the filesystem path of a unix address.
func SocketPath(addr string) string {
	p := strings.TrimPrefix(addr, "unix://")
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Listen opens the admin listener. A unix socket is created 0600 so its file
// permissions are the access control — the tightest option, since nothing on
// the network can reach it at all. A stale socket from an unclean shutdown is
// removed, but only after checking nothing is still serving on it.
func Listen(addr string) (net.Listener, string, error) {
	if !IsUnix(addr) {
		ln, err := net.Listen("tcp", addr)
		return ln, "http://" + addr, err
	}
	if !UnixSupported {
		return nil, "", errors.New(unixUnsupportedReason)
	}
	path := SocketPath(addr)
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, "", fmt.Errorf("another spillway is already serving %s", path)
	}
	// Nothing answered: any file there is a leftover.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	// The kernel caps sockaddr_un.sun_path (104 on darwin/BSD, 108 on
	// Linux) and reports an overrun as a bare "invalid argument", which
	// tells the user nothing about what to change.
	if max := sunPathMax(); len(path) > max {
		return nil, "", fmt.Errorf(
			"socket path is %d characters; this system allows %d — use a shorter path such as ~/.spillway.sock",
			len(path), max)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, "", fmt.Errorf("secure socket %s: %w", path, err)
	}
	return ln, path, nil
}

// sunPathMax is the kernel's limit on a unix socket path.
func sunPathMax() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103 // darwin and the BSDs
}

// IsLoopback reports whether addr binds only to the local machine. The
// implementation lives in netaddr because config gates proxy.host on the same
// question and cannot import this package.
func IsLoopback(addr string) bool { return netaddr.IsLoopback(addr) }

// GenerateToken returns a fresh per-start admin token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteTokenFile persists the token at 0600 (atomic).
func WriteTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

// ServeHTTP routes with the security middleware applied (§5).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding guard: loopback Host headers only.
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden: bad host", http.StatusForbidden)
		return
	}
	// A page embedding the dashboard cannot read it cross-origin anyway, but
	// there is no reason to allow the frame at all.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// CSRF guard on writes. The old rule — reject anything carrying Origin
	// or Sec-Fetch-Site — was written when nothing here could be written to.
	// It cannot stand now: the dashboard IS a browser page, so its own writes
	// carry both, and blanket rejection would make the settings endpoint
	// unusable from the UI it exists for.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !csrfOK(r) {
		http.Error(w, "forbidden: cross-site write rejected", http.StatusForbidden)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Everything except /api/settings, /api/pin and /api/accounts/remove is
	// read-only. The CSRF guard above is load-bearing from here on:
	// /api/settings is the first endpoint that writes, and a browser page
	// must not be able to drive it.
	// /api/pin writes too, and for the same reason as /api/settings it is
	// behind the CSRF guard rather than this one.
	// /api/accounts/remove is issue #83's live-removal endpoint: the CLI's
	// `accounts remove` has already edited the config and deleted the
	// credential by the time it calls this, so all this does is take the
	// name out of the running pool immediately.
	// /api/accounts/add is issue #87's mirror: the CLI's `login` has already
	// written the config and the secret store by the time it calls this.
	if r.URL.Path != "/api/settings" && r.URL.Path != "/api/pin" && r.URL.Path != "/api/accounts/remove" &&
		r.URL.Path != "/api/accounts/add" &&
		r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := staticFS.ReadFile("static/index.html")
		_, _ = w.Write(b)
	case r.URL.Path == "/api/accounts":
		s.writeJSON(w, s.accounts())
	case r.URL.Path == "/api/accounts/remove":
		s.handleAccountRemove(w, r)
	case r.URL.Path == "/api/accounts/add":
		s.handleAccountAdd(w, r)
	case r.URL.Path == "/logo.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		// Immutable for a day: it is the favicon, refetched on every tab.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		b, _ := staticFS.ReadFile("static/logo.svg")
		_, _ = w.Write(b)
	case r.URL.Path == "/api/state":
		s.handleState(w, r)
	case r.URL.Path == "/api/requests":
		s.handleRequests(w, r)
	case r.URL.Path == "/api/quota-history":
		s.handleQuotaHistory(w, r)
	case r.URL.Path == "/api/activity":
		s.handleActivity(w, r)
	case r.URL.Path == "/api/prefix-drift":
		s.handlePrefixDrift(w, r)
	case r.URL.Path == "/api/pin":
		s.handlePin(w, r)
	case r.URL.Path == "/api/settings":
		s.handleSettings(w, r)
	case r.URL.Path == eventsPath:
		s.handleEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if !s.requireToken {
		return true // loopback-only: see New
	}
	if s.token == "" {
		return false // required but absent: fail closed
	}
	if r.Header.Get("Authorization") == "Bearer "+s.token {
		return true
	}
	// A token in the query string is written into browser history and every
	// intermediary's access log, so it is accepted on one endpoint only: the
	// SSE stream, where EventSource gives the page no way to set a header.
	// Everything else the dashboard calls uses plain fetch and sends the
	// bearer header, so nothing else needs this.
	return r.URL.Path == eventsPath && r.URL.Query().Get("token") == s.token
}

// csrfOK decides whether a write may proceed.
//
// Sec-Fetch-Site is set by the browser and cannot be forged by page script,
// which makes it the reliable signal: "same-origin" is our own dashboard,
// anything else is another site driving the user's browser at our loopback
// port. Non-browser clients (curl, the CLI) send neither header and are
// allowed — they are not the CSRF threat, and the listener is loopback-only.
func csrfOK(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		// none: a user-initiated navigation rather than a page-driven fetch.
		return originIsSelf(r)
	case "":
		// No fetch-metadata: an old browser or a non-browser client. Fall
		// back to Origin, which a cross-site page cannot spoof either.
		return originIsSelf(r)
	default:
		return false // cross-site, same-site
	}
}

// originIsSelf accepts a missing Origin (non-browser clients omit it) and
// otherwise requires it to be this loopback listener.
func originIsSelf(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return loopbackHost(u.Host)
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")
}

// accountJSON is one account in the /api/accounts response.
type accountJSON struct {
	Name           string     `json:"name"`
	Label          string     `json:"label,omitempty"`
	Type           string     `json:"type"`
	Source         string     `json:"source"`
	State          string     `json:"state"`
	ExhaustedUntil *time.Time `json:"exhaustedUntil,omitempty"`
	InFlight       int        `json:"inFlight"`
	// OverThreshold marks an account the selector will rotate away from even
	// though nothing has 429'd it — its own quota headers say it is finished.
	// Without this it renders as healthy while spillway quietly avoids it.
	//
	// This is the GENERAL windows only (5h/7d), the same ones an
	// unrecognised or ordinary Sonnet/Opus request is judged against (issue
	// #24) — not "any window", which is what let a spent fable bucket mark
	// an otherwise-healthy account as done for every request. See
	// FableSpent for the narrower family.
	OverThreshold bool `json:"overThreshold,omitempty"`
	// FableSpent names the one other family spillway currently knows about
	// (§24 decision 3): true when the account's fable-specific weekly
	// bucket (7d-fable) is over threshold, independently of OverThreshold.
	// An account can be OverThreshold=false, FableSpent=true — fine for
	// Sonnet/Opus, done for fable — and the two bits are how the dashboard
	// says that instead of collapsing both families into one "spent" flag.
	FableSpent bool `json:"fableSpent,omitempty"`
	// Overage: the provider says extra usage is available on this account.
	// Paid is true only when it would actually be spent — available AND
	// permitted by config.
	Overage       bool   `json:"overage,omitempty"`
	OverageReason string `json:"overageReason,omitempty"`
	Paid          bool   `json:"paid,omitempty"`
	// OverageUsed is the fraction of the extra-usage allowance consumed
	// (0-1); -1 when the provider does not report it. At 1 the next billed
	// request is refused, and unlike a quota window this refills on a
	// billing period, so it can be months away — worth seeing coming.
	OverageUsed    float64            `json:"overageUsed,omitempty"`
	OverageResetAt *time.Time         `json:"overageResetAt,omitempty"`
	Priority       int                `json:"priority"`
	LastModel      string             `json:"lastModel,omitempty"`
	Upstream       string             `json:"upstream,omitempty"`
	Windows        []pool.QuotaWindow `json:"quotaWindows,omitempty"`
	// CacheHitRate, CacheCreateTokens and CacheReadTokens (issue #110) are
	// lifetime totals from the request log — cache_read/(cache_read +
	// cache_creation) and the two volumes themselves, so a burn spike is
	// explicable beside burn/h and dry-in rather than a mystery. Hit rate is
	// omitted (nil) rather than 0 when there is no cache volume yet.
	CacheHitRate      *float64 `json:"cacheHitRate,omitempty"`
	CacheCreateTokens int64    `json:"cacheCreateTokens,omitempty"`
	CacheReadTokens   int64    `json:"cacheReadTokens,omitempty"`
}

func (s *Server) accounts() []accountJSON {
	// Looked up once per call, not once per account: CacheStats already
	// aggregates every account in a single query.
	cacheByAcct := map[string]reqlog.CacheStat{}
	if s.log != nil {
		if stats, err := s.log.CacheStats(); err == nil {
			for _, c := range stats {
				cacheByAcct[c.Account] = c
			}
		}
		// Errors are swallowed deliberately: cache stats are a dashboard
		// nicety, not something worth failing /api/accounts over.
	}

	var out []accountJSON
	for _, a := range s.pool.Accounts() {
		j := accountJSON{
			Name:          a.Name,
			Label:         a.Label(),
			Type:          a.Type,
			Source:        a.Source,
			InFlight:      a.InFlight(),
			OverThreshold: a.OverThresholdFor("", s.pool.Threshold()),
			FableSpent:    a.OverThresholdForWindow("7d-fable", s.pool.Threshold()),
			Overage:       a.Overage().Available,
			OverageReason: a.Overage().Reason,
			OverageUsed:   a.Overage().Utilization,
			Paid:          a.CanOverage(s.pool.AllowOverage()),
			Priority:      a.Priority(),
			LastModel:     a.LastModel(),
			Upstream:      a.Upstream,
			Windows:       a.QuotaWindows(),
		}
		if c, ok := cacheByAcct[a.Name]; ok {
			j.CacheHitRate = c.HitRate()
			j.CacheCreateTokens = c.CacheCreationInputTokens
			j.CacheReadTokens = c.CacheReadInputTokens
		}
		if r := a.Overage().ResetAt; !r.IsZero() {
			j.OverageResetAt = &r
		}
		if a.Parked() {
			j.State = "parked"
			out = append(out, j)
			continue
		}
		switch a.State() {
		case pool.StateOK:
			j.State = "ok"
		case pool.StateExhausted:
			j.State = "exhausted"
			if u := a.ExhaustedUntil(); !u.IsZero() {
				j.ExhaustedUntil = &u
			}
		case pool.StateDisabled:
			j.State = "disabled"
		}
		out = append(out, j)
	}
	return out
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if s.log == nil {
		s.writeJSON(w, []reqlog.Entry{})
		return
	}
	entries, err := s.log.Recent(limit)
	if err != nil {
		http.Error(w, "request log error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []reqlog.Entry{}
	}
	s.writeJSON(w, entries)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)

	// Notable events can be minutes apart; a periodic comment keeps the stream
	// detectably alive (and stops idle intermediaries dropping it).
	beat := time.NewTicker(sseHeartbeat)
	defer beat.Stop()

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// hoursParam reads a bounded ?hours= window (default 6, max 168).
func hoursParam(r *http.Request) time.Duration {
	h, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || h < 1 {
		h = 6
	}
	if h > 168 {
		h = 168
	}
	return time.Duration(h) * time.Hour
}

// series is one account+window headroom curve.
type series struct {
	Account string    `json:"account"`
	Window  string    `json:"window"`
	Ts      []int64   `json:"ts"`       // epoch ms
	Head    []float64 `json:"headroom"` // 0..1 remaining
}

func (s *Server) handleQuotaHistory(w http.ResponseWriter, r *http.Request) {
	if s.log == nil {
		s.writeJSON(w, []series{})
		return
	}
	samples, err := s.log.QuotaSince(time.Now().Add(-hoursParam(r)))
	if err != nil {
		http.Error(w, "read quota history", http.StatusInternalServerError)
		return
	}
	idx := map[string]int{}
	out := []series{}
	for _, sm := range samples {
		if sm.Limit <= 0 {
			continue // no limit reported: headroom is undefined, not 100%
		}
		key := sm.Account + "\x00" + sm.Window
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, series{Account: sm.Account, Window: sm.Window})
		}
		head := 1 - sm.Used/sm.Limit
		if head < 0 {
			head = 0
		}
		out[i].Ts = append(out[i].Ts, sm.Ts.UnixMilli())
		out[i].Head = append(out[i].Head, head)
	}
	s.writeJSON(w, out)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if s.log == nil {
		s.writeJSON(w, []reqlog.Bucket{})
		return
	}
	span := hoursParam(r)
	now := time.Now()
	// Keep the histogram near 60 bars whatever the span.
	width := span / 60
	if width < time.Minute {
		width = time.Minute
	}
	b, err := s.log.Activity(now.Add(-span), width, now)
	if err != nil {
		http.Error(w, "read activity", http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, b)
}

// handlePrefixDrift serves issue #111 phase 1's measurement: how often each
// part of the request prefix changed between consecutive requests in a
// session, and what cache-creation volume went with it, split by whether
// the account changed too. Read-only, same shape as every other diagnostic
// endpoint here — no window parameter, because the requests table is not
// pruned and the whole point is a body of evidence large enough to decide
// whether #111's transforms are worth building.
func (s *Server) handlePrefixDrift(w http.ResponseWriter, r *http.Request) {
	if s.log == nil {
		s.writeJSON(w, []reqlog.PrefixChange{})
		return
	}
	out, err := s.log.PrefixDrift()
	if err != nil {
		http.Error(w, "read prefix drift", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []reqlog.PrefixChange{}
	}
	s.writeJSON(w, out)
}

// handleSettings reads and writes the editable configuration subset.
// Credentials are not part of that subset and never will be: token material
// must not be reachable from a browser, loopback or not.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" || s.onSettings == nil {
		http.Error(w, "settings editing not enabled", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := config.LoadFrom(s.configPath)
		if err != nil {
			http.Error(w, "read config", http.StatusInternalServerError)
			return
		}
		s.writeJSON(w, config.CurrentSettings(cfg))

	case http.MethodPut:
		var in config.Settings
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "malformed request body", http.StatusBadRequest)
			return
		}
		cfg, err := config.UpdateSettings(s.configPath, in)
		if err != nil {
			// A rejected value is the user's typo, not a server fault: give
			// them the message rather than a 500.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Apply to the running pool so the change takes effect without a
		// restart — a restart drops the SSE stream and re-probes every
		// account.
		s.onSettings(cfg)
		s.writeJSON(w, config.CurrentSettings(cfg))

	case http.MethodPost:
		// Issue #83: re-apply whatever is already on disk to the running
		// pool, with no body. This is what the CLI calls after writing a
		// mutation PUT can't express — `accounts priority`/`accounts
		// overage` write straight to the yaml via config.SetAccountPriority
		// /SetAccountOverage (overage deliberately bypasses UpdateSettings;
		// see that function's doc), so there is no new Settings payload to
		// PUT — only an instruction to pick up what the file now says. Same
		// mechanism as PUT's tail end: reread, then s.onSettings(cfg), which
		// is the one place pool.Apply is called from here.
		cfg, err := config.LoadFrom(s.configPath)
		if err != nil {
			http.Error(w, "read config", http.StatusInternalServerError)
			return
		}
		s.onSettings(cfg)
		s.writeJSON(w, config.CurrentSettings(cfg))

	default:
		w.Header().Set("Allow", "GET, PUT, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}
