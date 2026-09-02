package accounts

// Loopback OAuth callback: the browser hands the code straight back, instead
// of the user copying `code#state` out of a web page.
//
// The redirect URI is fixed rather than an ephemeral port. Only
// http://localhost:54545/callback is known to be registered for Claude
// Code's client — verified by loading the authorize URL and getting the
// consent screen rather than a redirect_uri error. RFC 8252 says providers
// SHOULD accept any loopback port, but "should" is not "does", and a port
// the provider rejects fails in the browser after the user has already
// committed to the flow. If the port is busy, fall back to paste, which
// always works.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// LoopbackPort is the one port confirmed registered for this client.
	LoopbackPort = 54545
	// LoopbackRedirectURI must match the authorize request byte for byte —
	// the token exchange sends it again and the provider compares.
	LoopbackRedirectURI = "http://localhost:54545/callback"
)

// CallbackServer waits for the browser to deliver an authorization code.
type CallbackServer struct {
	lns []net.Listener
	srv *http.Server
	got chan callbackResult
	uri string

	// mu guards seen. Every bound family serves the same handler, so two
	// callbacks arriving at once run finish on two goroutines.
	mu   sync.Mutex
	seen bool
}

type callbackResult struct {
	code string
	err  error
}

// loopbackFamilies reports which loopback address families this call to
// StartCallback must bind. It is a package variable, not a hard-coded call
// inside StartCallback, purely so tests can force a single-family host
// without StartCallback itself knowing it is under test.
var loopbackFamilies = probeLoopbackFamilies

// probeLoopbackFamilies determines which of 127.0.0.1 and ::1 this host can
// actually serve loopback traffic on.
//
// It must NOT probe by binding LoopbackPort itself: that would either race
// the bind StartCallback is about to make (two goroutines/processes both
// trying the real port at once) or falsely report the port as busy because
// of the probe's own listener sitting on it. Instead each candidate is
// bound on an ephemeral port (":0") and immediately closed. Whether the
// kernel will hand out *a* socket on that address at all is a property of
// the host's network stack (is the IPv6 loopback interface up, etc.),
// independent of which port number is requested — so probing on :0 answers
// the "can this host do this family" question honestly without ever
// touching :54545.
func probeLoopbackFamilies() []string {
	var supported []string
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		ln, err := net.Listen("tcp", host+":0")
		if err != nil {
			continue
		}
		ln.Close()
		supported = append(supported, host)
	}
	return supported
}

// StartCallback binds the loopback listener. A bind failure is not fatal to
// login — the caller falls back to paste — so the error explains rather than
// stops.
//
// "localhost" resolves to 127.0.0.1 or ::1 depending on the machine and the
// resolver's preference, and the redirect URI has to match whichever the
// browser actually uses. So this binds every family the host supports, not
// a best-effort subset: probeLoopbackFamilies (or its test override) says
// what the host can do, and every one of those must bind here or the whole
// call fails. A host that only has one family still gets a working server
// on that family; a host with both must get both, because a partial bind
// leaves the other family's traffic reaching nothing — or, worse, another
// login's server.
func StartCallback(state string) (*CallbackServer, error) {
	cs := &CallbackServer{got: make(chan callbackResult, 1), uri: LoopbackRedirectURI}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The provider's own error (user declined, bad scope) reaches us
		// here; passing it through beats a timeout with no explanation.
		if e := q.Get("error"); e != "" {
			cs.finish(w, callbackResult{err: fmt.Errorf("authorization failed: %s: %s",
				e, q.Get("error_description"))})
			return
		}
		// Constant-time, and required: without it any page the user visits
		// while this server is up could drive a code of its own into it.
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			cs.finish(w, callbackResult{err: errors.New("state mismatch — " +
				"the callback did not come from the login that was started")})
			return
		}
		code := q.Get("code")
		if code == "" {
			cs.finish(w, callbackResult{err: errors.New("callback carried no authorization code")})
			return
		}
		cs.finish(w, callbackResult{code: code})
	})
	// Anything else is not ours. A blank 404 rather than a hint about what
	// is running here.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	families := loopbackFamilies()
	if len(families) == 0 {
		return nil, fmt.Errorf("no loopback address family available on this host")
	}

	for _, host := range families {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, LoopbackPort))
		if err != nil {
			// This family was just probed as available on an ephemeral
			// port, so failing on the real one means something else is
			// already bound to it — not that the host lacks the family.
			for _, opened := range cs.lns {
				opened.Close()
			}
			return nil, fmt.Errorf("cannot listen on localhost:%d (in use?): %w", LoopbackPort, err)
		}
		cs.lns = append(cs.lns, ln)
	}

	cs.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	for _, ln := range cs.lns {
		go func(l net.Listener) { _ = cs.srv.Serve(l) }(ln)
	}
	return cs, nil
}

// finish answers the browser and delivers the result once. A second callback
// (a refresh, a duplicated tab) must not overwrite the first.
//
// The once-only check is locked because concurrent callbacks are ordinary
// here, not exotic: both loopback families are bound to the same handler, so
// a browser that tries 127.0.0.1 and ::1, prefetches the URL, or simply has
// the tab restored can run finish twice at once. Unlocked, both goroutines
// could read seen as false and both send — and got holds one, so the second
// send blocks forever, leaking its handler goroutine and hanging the
// Shutdown in Close behind that still-active connection. Sending under the
// lock is safe for the same reason: exactly one send ever reaches a channel
// buffered for one, so it cannot block while holding it.
func (cs *CallbackServer) finish(w http.ResponseWriter, res callbackResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This page is the only part of spillway a browser ever renders besides
	// the dashboard, so it uses the dashboard's own tokens rather than
	// looking like a different program's error page.
	w.Header().Set("Referrer-Policy", "no-referrer")
	fmt.Fprint(w, callbackPage(res.err))

	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.seen {
		return
	}
	cs.seen = true
	cs.got <- res
}

// callbackPage renders the one page a browser sees from spillway besides the
// dashboard.
//
// Self-contained: no fonts, no stylesheets, no scripts. It is served by a
// listener that exists for a few seconds and then stops, so anything fetched
// from elsewhere is latency on the only screen the user is waiting for — and
// a request to a third party at the exact moment they finish authenticating.
//
// Renders NOTHING from the request. The code and state arrive as URL
// parameters and the browser keeps that page in history; err is spillway's
// own message, and it is escaped anyway.
func callbackPage(err error) string {
	title, note, tone := "Signed in", "You can close this tab and go back to the terminal.", "ok"
	if err != nil {
		title, note, tone = "Login failed", err.Error(), "bad"
	}
	return `<!doctype html><html lang="en"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>spillway</title>
<style>
  :root {
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
    --deep: #eef4f6; --surface: #fff; --wall: #d3e1e7;
    --ink: #08202a; --ink-2: #567480;
    --water-a: #35b6d8; --water-b: #1a7ea6; --critical: #d03b3b;
    --shadow: 0 1px 2px rgba(8,32,42,.05), 0 10px 30px -22px rgba(8,32,42,.5);
    --swell: url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 100' preserveAspectRatio='none'><path d='M0 36 C78 36 128 0 236 0 C344 0 410 36 500 36 L500 100 L0 100 Z' fill='%23fff'/></svg>");
    color-scheme: light;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --deep: #061219; --surface: #0e212b; --wall: #1c3a49;
      --ink: #dcebf0; --ink-2: #7794a1;
      --water-a: #3fc6e8; --water-b: #12688a; --shadow: none;
      color-scheme: dark;
    }
  }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         padding: 1.5rem; background: var(--deep); color: var(--ink);
         font: 14px/1.55 var(--sans); letter-spacing: -.005em;
         -webkit-font-smoothing: antialiased; }
  .card { background: var(--surface); border: 1px solid var(--wall);
          border-radius: 14px; box-shadow: var(--shadow);
          padding: 1.9rem 2rem; max-width: 26rem; width: 100%; text-align: center; }
  .mark { font-size: 15px; font-weight: 700; letter-spacing: -.045em;
          margin: 0 0 1.5rem; color: var(--ink); }
  h1 { font-size: 17px; font-weight: 600; letter-spacing: -.02em; margin: 0 0 .45rem; }
  p { margin: 0; color: var(--ink-2); font-size: 13px; overflow-wrap: anywhere; }
  /* The tank, filling. Same object the dashboard is built around, at the one
     moment an account actually becomes available. */
  .tank { width: 46px; height: 62px; margin: 0 auto 1.3rem; position: relative;
          border-radius: 6px; overflow: hidden; background: var(--deep);
          border: 1px solid var(--wall); }
  .fill { position: absolute; left: 0; right: 0; bottom: 0; height: 0;
          background: linear-gradient(180deg, var(--water-a), var(--water-b));
          animation: rise .9s cubic-bezier(.22,.7,.3,1) forwards; }
  /* The same crest the dashboard's tanks use, so this reads as the same
     object rather than a flat blue rectangle. Sits ON the waterline, which
     is why it is offset by its own height. */
  .fill::before {
    content: ""; position: absolute; left: 0; right: 0; bottom: 100%; height: 7px;
    background: var(--water-a);
    -webkit-mask: var(--swell) repeat-x bottom / 60px 7px;
    mask: var(--swell) repeat-x bottom / 60px 7px;
  }
  .bad .fill { background: var(--critical); animation: none; height: 14%; opacity: .65; }
  .bad .fill::before { background: var(--critical); }
  @keyframes rise { to { height: 72%; } }
  @media (prefers-reduced-motion: reduce) { .fill { animation: none; height: 72%; } }
</style>
<body>
  <main class="card ` + tone + `">
    <p class="mark">spillway</p>
    <div class="tank"><div class="fill"></div></div>
    <h1>` + htmlEscape(title) + `</h1>
    <p>` + htmlEscape(note) + `</p>
  </main>
`
}

// RedirectURI is what must be sent in both the authorize request and the
// token exchange.
func (cs *CallbackServer) RedirectURI() string { return cs.uri }

// Wait blocks for the code, the context, or the timeout.
func (cs *CallbackServer) Wait(ctx context.Context, timeout time.Duration) (string, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case res := <-cs.got:
		return res.code, res.err
	case <-t.C:
		return "", fmt.Errorf("timed out after %s waiting for the browser", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close stops the server and releases the port.
func (cs *CallbackServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cs.srv.Shutdown(ctx)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
