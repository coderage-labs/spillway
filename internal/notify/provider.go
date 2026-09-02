package notify

// Providers deliver one Notification to one channel's Destination (issue
// #101). This is deliberately not a plugin system: a small registry keyed by
// a provider string is the right size, and every provider here is a thin
// wrapper around a single HTTP POST.
//
// Message content is kept to state and timing, never identity: an ntfy
// topic on the free tier has no access control at all (anyone who guesses it
// can read AND publish to it), so a leaked topic must not be worth reading.
// See ChannelKey and the README for the same reasoning applied to how a
// topic should be generated.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notification is one message ready to deliver. Providers must not log Dest
// — only the channel name, which callers already have — Notification itself
// carries nothing secret.
type Notification struct {
	Event string
	Key   string
	Title string
	Body  string
}

// Destination is one channel's credential material: the whole reason a
// channel exists is secret (issue #101, §5 — config holds metadata only),
// so this is never read from config, only from the secret store under
// ChannelKey(name). Which fields a given provider reads depends on the
// provider; unused fields are ignored.
type Destination struct {
	// URL is the webhook target (webhook), or a self-hosted ntfy base
	// (ntfy) — ntfy.sh is used when empty.
	URL string `json:"url,omitempty"`
	// Topic is the ntfy topic. On ntfy.sh this alone is the credential:
	// anyone who knows it can read AND publish to it.
	Topic string `json:"topic,omitempty"`
	// Token is a bearer credential: an ntfy access token (reserved topic /
	// self-hosted, "tk_..." — issue #101 comment) or a webhook's
	// Authorization header. Both send it the same way, so one field covers
	// both providers.
	Token string `json:"token,omitempty"`
	// UserKey is Pushover's user key (Token there is the app token).
	UserKey string `json:"userKey,omitempty"`
}

// Empty reports whether no credential material is set at all — used to
// treat "channel configured but never given a secret" as a missing
// credential rather than a working, empty one.
func (d Destination) Empty() bool {
	return d.URL == "" && d.Topic == "" && d.Token == "" && d.UserKey == ""
}

// Provider delivers one Notification to one Destination. Send must respect
// ctx's deadline and return a plain error — callers log it once (naming the
// channel, never the destination) and move on; nothing here is retried
// (Pushover's own emergency-priority repeat is a server-side behaviour, not
// a client retry).
type Provider interface {
	Send(ctx context.Context, dest Destination, n Notification) error
}

// httpProviderTimeout bounds a single provider HTTP call. Notify's caller
// (deliver) also applies this as the context deadline, so this constant is
// really documentation of what that deadline is for.
const httpProviderTimeout = 10 * time.Second

var sharedHTTPClient = &http.Client{Timeout: httpProviderTimeout}

// providerFor returns the Provider for a registered name, or ok=false for
// anything else. osSend/osEnabled thread through the platform-local sender
// so "os" is backed by the exact same code path notify.go always used.
func providerFor(name string, osSend func(ctx context.Context, title, body string) error, osEnabled bool) (p Provider, enabled bool, ok bool) {
	switch name {
	case "os":
		return &osProvider{send: osSend}, osEnabled, true
	case "webhook":
		return &webhookProvider{client: sharedHTTPClient}, true, true
	case "ntfy":
		return &ntfyProvider{client: sharedHTTPClient}, true, true
	case "pushover":
		return &pushoverProvider{client: sharedHTTPClient}, true, true
	default:
		return nil, false, false
	}
}

// osProvider adapts the platform-local notifier as a Provider, so "os" is a
// channel like any other rather than a special case beside them.
type osProvider struct {
	send func(ctx context.Context, title, body string) error
}

func (p *osProvider) Send(ctx context.Context, _ Destination, n Notification) error {
	return p.send(ctx, n.Title, n.Body)
}

// webhookProvider posts a generic JSON body. The shape deliberately covers
// several consumers at once rather than picking one: Slack reads "text",
// Discord reads "content", and title/body/event are there for anything
// else (ntfy also accepts a JSON body, but has its own provider below for
// the topic/title-header convention).
type webhookProvider struct{ client *http.Client }

func (p *webhookProvider) Send(ctx context.Context, dest Destination, n Notification) error {
	if dest.URL == "" {
		return fmt.Errorf("webhook: no url configured")
	}
	text := n.Title + "\n" + n.Body
	payload, err := json.Marshal(map[string]string{
		"text": text, "content": text,
		"title": n.Title, "body": n.Body, "event": n.Event,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if dest.Token != "" {
		req.Header.Set("Authorization", "Bearer "+dest.Token)
	}
	return doAndCheck(p.client, req)
}

// ntfyProvider posts to an ntfy topic: ntfy.sh by default, or a self-hosted
// base URL (dest.URL). The topic is the whole credential on the free tier
// (issue #101 comment) — a bearer token is supported for a reserved topic
// or self-hosted deployment, the same Authorization header the webhook
// provider uses.
type ntfyProvider struct{ client *http.Client }

// ntfySh is the hosted default base URL, used whenever a channel doesn't
// name a self-hosted one.
const ntfySh = "https://ntfy.sh"

// ntfyTargetURL builds the publish URL for dest, defaulting to ntfySh.
// Split out so the default can be checked without a network round trip.
func ntfyTargetURL(dest Destination) string {
	base := dest.URL
	if base == "" {
		base = ntfySh
	}
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(dest.Topic)
}

func (p *ntfyProvider) Send(ctx context.Context, dest Destination, n Notification) error {
	if dest.Topic == "" {
		return fmt.Errorf("ntfy: no topic configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ntfyTargetURL(dest), strings.NewReader(n.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", n.Title)
	if dest.Token != "" {
		req.Header.Set("Authorization", "Bearer "+dest.Token)
	}
	return doAndCheck(p.client, req)
}

// pushoverProvider posts to Pushover's messages API. Priority is fixed at
// emergency (2, retried by Pushover's own servers until acknowledged): that
// repeat-until-ack behaviour is the one thing that actually wakes someone,
// and it is Pushover's whole reason for existing in this list (issue #101).
const pushoverAPI = "https://api.pushover.net/1/messages.json"

type pushoverProvider struct {
	client *http.Client
	// api overrides pushoverAPI; empty means the real endpoint. Tests only.
	api string
}

func (p *pushoverProvider) Send(ctx context.Context, dest Destination, n Notification) error {
	if dest.Token == "" || dest.UserKey == "" {
		return fmt.Errorf("pushover: token and user key both required")
	}
	form := url.Values{
		"token":    {dest.Token},
		"user":     {dest.UserKey},
		"title":    {n.Title},
		"message":  {n.Body},
		"priority": {"2"},
		"retry":    {"60"},
		"expire":   {"3600"},
	}
	api := p.api
	if api == "" {
		api = pushoverAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doAndCheck(p.client, req)
}

// SendLocal delivers one notification via the platform-local notifier
// directly, bypassing dedup and channels entirely. Used by `spillway notify
// test` to give an "os" channel the same honest, synchronous send every
// other provider gets.
func SendLocal(ctx context.Context, title, body string) error {
	n := New()
	if !n.Enabled {
		return fmt.Errorf("no local notifier available on this platform")
	}
	return n.send(ctx, title, body)
}

// Send resolves providerName and delivers n to dest synchronously — the
// same registry Notify's fan-out uses, exposed directly for `spillway
// notify test`, which wants an honest success/failure rather than a
// fire-and-forget log line.
func Send(ctx context.Context, providerName string, dest Destination, n Notification) error {
	if providerName == "os" {
		return SendLocal(ctx, n.Title, n.Body)
	}
	p, _, ok := providerFor(providerName, nil, false)
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}
	return p.Send(ctx, dest, n)
}

func doAndCheck(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		// client.Do's error is a *url.Error, whose Error() string embeds
		// the full request URL ("Post \"https://…/<secret-topic>\": dial
		// tcp: …") — exactly the destination material that must never be
		// logged (deliver logs this error verbatim). A DNS or TLS failure
		// message can carry the same hostname. Never propagate the
		// underlying text; a channel name plus this generic reason is
		// everything a log line is allowed to say.
		return errors.New("request failed (network or TLS error)")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
