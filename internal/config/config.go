// Package config loads and validates spillway's YAML configuration.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/netaddr"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/provider"
	"gopkg.in/yaml.v3"
)

// AccountConfig is one pooled account. Credentials are either inline
// (imported) or resolved at startup from the local keychain.
type AccountConfig struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`             // "claude-oauth" / "kimi-oauth"
	Source string `yaml:"source,omitempty"` // "keychain" for the local claude login
	// Label is what the dashboard shows instead of the account name. Two
	// accounts on the same address differ only by domain, which makes for
	// poor display names; this lets them be called what the user calls them.
	Label string `yaml:"label,omitempty"`
	// Disabled removes an account from rotation without deleting it or its
	// credential — the reversible way to park one.
	Disabled bool `yaml:"disabled,omitempty"`
	// Priority orders selection: lower is preferred. Accounts of equal
	// priority are chosen by load, as before. Use it to spend a cheaper or
	// more expendable account first, or to keep one in reserve.
	Priority int `yaml:"priority,omitempty"`
	// AllowOverage overrides pool.allowOverage for this account. Unset means
	// follow the pool. Set it false on an account billed to someone else.
	AllowOverage *bool             `yaml:"allowOverage,omitempty"`
	AccessToken  string            `yaml:"accessToken,omitempty"`
	RefreshToken string            `yaml:"refreshToken,omitempty"`
	ExpiresAt    int64             `yaml:"expiresAt,omitempty"` // epoch milliseconds
	AccountUUID  string            `yaml:"accountUuid,omitempty"`
	Upstream     string            `yaml:"upstream,omitempty"` // defaults per provider
	ModelMap     map[string]string `yaml:"modelMap,omitempty"` // incoming model id → provider model id
}

// NotifyChannelConfig is one push-notification destination's metadata
// (issue #101, §5: config holds metadata only). The credential — a webhook
// URL, an ntfy topic, a Pushover token/user key — lives in the secret store
// under notify.ChannelKey(Name), never here.
type NotifyChannelConfig struct {
	Name     string   `yaml:"name"`
	Provider string   `yaml:"provider"`
	Events   []string `yaml:"events,omitempty"`
	// The fields below are never written by spillway itself — `spillway
	// notify set` writes straight to the secret store. They exist on this
	// struct only so a value someone hand-edits into the yaml is actually
	// read (rather than silently ignored by yaml.Unmarshal) and can be
	// caught and scrubbed by MigrateInlineSecrets, mirroring
	// AccountConfig's AccessToken/RefreshToken above.
	URL     string `yaml:"url,omitempty"`
	Topic   string `yaml:"topic,omitempty"`
	Token   string `yaml:"token,omitempty"`
	UserKey string `yaml:"userKey,omitempty"`
}

// Config is the root of ~/.config/spillway.yaml.
type Config struct {
	Proxy struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
		// AllowRemote is the opt-in required to bind the proxy anywhere but
		// loopback. The proxy port is unauthenticated and injects a pooled
		// account's credential into everything it forwards, so a non-loopback
		// bind hands the quota — and with allowOverage, the money — to anyone
		// who can reach it, along with every prompt in flight. Validate
		// refuses that bind without this flag; see the guard there.
		AllowRemote bool `yaml:"allowRemote,omitempty"`
	} `yaml:"proxy"`
	Upstream string          `yaml:"upstream"`
	Accounts []AccountConfig `yaml:"accounts,omitempty"`
	// Admin is the dashboard listener. On loopback it needs no token (the
	// token would only defend against other users of this machine, while
	// putting a secret in the URL); binding anywhere else makes one
	// mandatory, generated if not supplied.
	// Egress is how spillway reaches upstreams (§6.13). Both the pooled
	// request path and the MITM tunnel honour it — proxying only one would
	// silently leak the other.
	Egress struct {
		Mode  string `yaml:"mode,omitempty"`  // direct | http-connect | environment
		Proxy string `yaml:"proxy,omitempty"` // required for http-connect
	} `yaml:"egress,omitempty"`

	Admin struct {
		Addr  string `yaml:"addr"`
		Token string `yaml:"token,omitempty"`
	} `yaml:"admin"`
	Pool struct {
		// ExhaustedMode: "fail" (429 straight through), "hold" (park until
		// reset), "notify" (hold + loud log). Default notify (§6.11).
		ExhaustedMode string `yaml:"exhaustedMode"`
		// HoldMax caps how long a request may be parked waiting for a quota
		// reset, e.g. "4h". "0" = never hold.
		HoldMax string `yaml:"holdMax"`
		// SwitchThreshold is the used-fraction (0-1] of any quota window at
		// or above which an account is skipped while another eligible
		// account exists (predictive rotation, §6.5). Default 0.98.
		SwitchThreshold float64 `yaml:"switchThreshold"`
		// AllowOverage permits serving from an account whose subscription
		// quota is exhausted but which has extra usage (pay-as-you-go)
		// available, as a last resort after every free account is spent.
		// Default false: this is the only setting that makes spillway spend
		// money, so it is opt-in rather than opt-out.
		AllowOverage bool `yaml:"allowOverage,omitempty"`
		// ProbeOnStart sends ONE minimal request per account that has no
		// quota data, so a standby account shows a level instead of sitting
		// blank. Costs a token per account per daemon start; set false to
		// keep spillway strictly a proxy that never originates traffic.
		ProbeOnStart *bool `yaml:"probeOnStart,omitempty"`
		// ProbeInterval re-probes accounts whose quota reading has gone
		// stale, so a standby tank does not show hours-old data. "0" probes
		// at startup only. One request per stale account per interval.
		ProbeInterval string `yaml:"probeInterval,omitempty"`
		// CrossProvider allows a session to rotate between providers (Claude
		// -> Kimi) mid-flight. Off by default: the client fixed its
		// capabilities from the first model it saw, so switching families
		// underneath it can change the context ceiling and break forced tool
		// calls (§6.18, §6.19). Same-provider rotation is unaffected.
		CrossProvider bool `yaml:"crossProvider,omitempty"`
		// StickyAcrossFamily keeps a session pinned to its sticky account
		// even when the model-family bucket that request needs is spent on
		// that account and another account has headroom (issue #24). Off by
		// default: spillway moves to the healthier account and the session
		// eats a cold prompt cache. Set true to keep the warm cache instead
		// and risk the upstream refusal that account's own bucket would
		// give — same-family requests where the sticky account still has
		// headroom are never affected either way.
		StickyAcrossFamily bool `yaml:"stickyAcrossFamily,omitempty"`
		// HideOverageFromClient strips Anthropic's credit markers — the
		// overage-in-use and overage-disabled-reason headers, the fable
		// representative-claim on a 429, and a 429 body's
		// "credits_required" code — from pooled Claude responses before
		// they reach the client (issue #103). Claude Code latches on those
		// signals and silently swaps a session's model down for good, even
		// after another pooled account's window has reset; behind a pool
		// they describe one account, not the session's real headroom.
		//
		// Off by default, and turning it on is a consent decision, not a
		// tuning knob: it removes the client's own paid-usage dialog, so
		// AllowOverage above (off by default, fail-closed, per-account
		// overridable) becomes the ONLY thing standing between a spent pool
		// and a billed request. Spillway's own spending record is
		// unaffected either way — the overage warning, notification and
		// request-log entry are written before the strip.
		HideOverageFromClient bool `yaml:"hideOverageFromClient,omitempty"`
		// MaxBufferBytes caps the request body held for cross-account retry.
		// Larger bodies stream straight through with no failover, so this
		// trades memory for how big a request can still be retried.
		MaxBufferBytes int `yaml:"maxBufferBytes,omitempty"`
		// CanaryInterval checks idle accounts for credentials that have died
		// without any request failing. "0" disables it.
		CanaryInterval string `yaml:"canaryInterval,omitempty"`
	} `yaml:"pool"`
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
	// WatchConfig makes a running daemon pick this file up when anything
	// other than spillway's own CLI edits it — a text editor, a script, a
	// synced dotfile (issue #84). Default on: the product requirement is
	// that a restart is a last resort, and a config that only takes effect
	// after one is the commonest way that promise breaks. Set false to opt
	// out; the daemon then behaves exactly as it did before #84 and only
	// picks the file up at startup or when the CLI/dashboard tells it to.
	//
	// A pointer so "absent" and "explicitly false" are distinguishable —
	// same tri-state reason as Pool.ProbeOnStart.
	WatchConfig *bool `yaml:"watchConfig,omitempty"`
	// Notify configures push-notification channels (issue #101). Off by
	// default: no channels block means today's behaviour exactly — local
	// desktop notifications only.
	Notify struct {
		Channels []NotifyChannelConfig `yaml:"channels,omitempty"`
	} `yaml:"notify,omitempty"`
}

// DefaultProxyPort and DefaultProxyHost are the listener written on first
// run. Named because more than one place has to reconstruct the same address
// — the status line decides whether the client that invoked it is pointed
// here, and comparing against a different literal would answer wrongly.
const (
	DefaultProxyPort = 7654
	DefaultProxyHost = "127.0.0.1"
)

// Defaults returns the configuration written on first run.
func Defaults() Config {
	var c Config
	c.Proxy.Port = DefaultProxyPort
	c.Proxy.Host = DefaultProxyHost
	c.Upstream = "https://api.anthropic.com"
	c.Admin.Addr = "127.0.0.1:7657"
	c.Pool.ExhaustedMode = "notify"
	c.Pool.HoldMax = "4h"
	c.Pool.SwitchThreshold = 0.98
	probe := true
	c.Pool.ProbeOnStart = &probe
	c.Pool.ProbeInterval = "30m"
	c.Pool.MaxBufferBytes = 8 << 20
	c.Pool.CanaryInterval = "2h"
	c.Log.Level = "info"
	watch := true
	c.WatchConfig = &watch
	return c
}

// WatchEnabled reports whether the daemon should watch this file for
// external edits (issue #84). Absent means yes — see WatchConfig.
func (c *Config) WatchEnabled() bool {
	return c.WatchConfig == nil || *c.WatchConfig
}

// PoolProbeInterval parses Pool.ProbeInterval; 0 means startup-only.
func (c *Config) PoolProbeInterval() time.Duration {
	if c.Pool.ProbeInterval == "" {
		return 30 * time.Minute
	}
	if c.Pool.ProbeInterval == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.Pool.ProbeInterval)
	if err != nil || d < 0 {
		return 30 * time.Minute
	}
	return d
}

// PoolCanaryInterval parses Pool.CanaryInterval.
func (c *Config) PoolCanaryInterval() time.Duration {
	d, err := time.ParseDuration(c.Pool.CanaryInterval)
	if err != nil || d <= 0 {
		return 2 * time.Hour
	}
	return d
}

// PoolHoldMax parses Pool.HoldMax ("4h", "30m", "0") into a duration.
func (c *Config) PoolHoldMax() time.Duration {
	if c.Pool.HoldMax == "" || c.Pool.HoldMax == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.Pool.HoldMax)
	if err != nil {
		return 0
	}
	return d
}

// Path resolves the config file location: $SPILLWAY_CONFIG if set, else
// ~/.config/spillway.yaml.
func Path() (string, error) {
	if p := os.Getenv("SPILLWAY_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "spillway.yaml"), nil
}

// Load reads the config file, creating it with defaults (mode 0600) when it
// does not exist, then validates it.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom is Load for an explicit path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createDefaults(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// ParseValidate parses and validates config bytes without touching the
// filesystem at all (issue #84's watcher). Deliberately NOT LoadFrom: that
// creates the file with defaults when it is missing, which is right at
// startup and catastrophic for a watcher — a config deleted or momentarily
// absent underneath a running daemon would be silently replaced by defaults
// and then applied, emptying the running pool. It also parses exactly the
// bytes the caller already hashed, so there is no window between deciding
// the file has settled and reading it again.
func ParseValidate(data []byte) (*Config, error) {
	cfg, err := parse(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Fingerprint is a content hash of a config's MEANING, not of the bytes it
// came from: it hashes the canonical re-marshalling of a parsed config, so
// two files that differ only in key order, indentation, comments or omitted
// defaults fingerprint identically.
//
// Issue #84 uses this to not act on its own tail: the daemon rewrites this
// file itself (a refreshed token's expiry, a dashboard settings write), and
// a reload that re-applied every such rewrite would log noise forever and,
// worse, make "did anything actually change" unanswerable. Hashing raw
// bytes would not do — a formatting-only rewrite is not a change.
func Fingerprint(c *Config) [32]byte {
	data, err := yaml.Marshal(c)
	if err != nil {
		// Marshalling a Config cannot fail (no channels, funcs or cyclic
		// values in it). Hash the error text rather than returning a fixed
		// value, so an impossible failure can never read as "unchanged".
		return sha256.Sum256([]byte("spillway-fingerprint-error:" + err.Error()))
	}
	return sha256.Sum256(data)
}

func parse(data []byte) (*Config, error) {
	cfg := Defaults()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func createDefaults(path string) (*Config, error) {
	cfg := Defaults()
	if err := writeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// writeFile marshals cfg and writes it atomically at 0600 (tightening
// existing perms).
func writeFile(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config %s: %w", path, err)
	}
	// WriteFile's perm only applies on creation; enforce on existing files.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config %s: %w", path, err)
	}
	return nil
}

// UpdateAccountExpiry persists a refreshed token's expiry for one account
// back into the config file, atomically and at 0600. Token material itself
// lives in the secret store, never here.
func UpdateAccountExpiry(path, name string, expiresAt int64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == name {
			cfg.Accounts[i].ExpiresAt = expiresAt
			return writeFile(path, cfg)
		}
	}
	return fmt.Errorf("account %q not found in %s", name, path)
}

// Validate checks every field, returning a descriptive error.
func (c *Config) Validate() error {
	if c.Proxy.Port < 1 || c.Proxy.Port > 65535 {
		return fmt.Errorf("proxy.port: %d out of range 1-65535", c.Proxy.Port)
	}
	if c.Proxy.Host == "" {
		return errors.New("proxy.host: must not be empty")
	}
	// The proxy listener has no authentication of its own and stamps a pooled
	// account's bearer token onto every request it forwards. Off loopback
	// that is an open credential-injecting relay: anyone who can reach the
	// port spends the quota (the money, under allowOverage) and can read or
	// rewrite every prompt and response passing through. The admin listener
	// already fails closed on the same question by making its token mandatory
	// off loopback; this port holds more and so refuses outright unless the
	// operator says the words. Refusing at Validate means the daemon does not
	// start, rather than starting and quietly exposing the credential.
	if !netaddr.IsLoopback(c.Proxy.Host) && !c.Proxy.AllowRemote {
		return fmt.Errorf("proxy.host: %q is not loopback — the proxy port is "+
			"unauthenticated and injects a pooled account's credential into every "+
			"request, so binding it off this machine lets anyone who can reach it "+
			"spend your quota and read your prompts; use %s, or set proxy.allowRemote: true "+
			"if you really mean to expose it", c.Proxy.Host, DefaultProxyHost)
	}
	if err := validateUpstream("upstream", c.Upstream); err != nil {
		return err
	}
	seen := map[string]bool{}
	// Two entries for one provider account are not two accounts. The pool
	// would rotate between tanks backed by the same quota, and — the reason
	// this is an error rather than a warning — refresh the same credential
	// from two places, which is how a live token was lost early on.
	byUUID := map[string]string{}
	for i, a := range c.Accounts {
		where := fmt.Sprintf("accounts[%d] (%q)", i, a.Name)
		if a.Name == "" {
			return fmt.Errorf("%s: name must not be empty", where)
		}
		if seen[a.Name] {
			return fmt.Errorf("%s: duplicate account name", where)
		}
		seen[a.Name] = true
		if a.AccountUUID != "" {
			if prev, ok := byUUID[a.AccountUUID]; ok {
				return fmt.Errorf("%s: same provider account as %q (accountUuid %s) — "+
					"remove one with `spillway accounts remove %s`",
					where, prev, a.AccountUUID, a.Name)
			}
			byUUID[a.AccountUUID] = a.Name
		}
		if !provider.Known(a.Type) {
			return fmt.Errorf("%s: type %q must be one of %s", where, a.Type,
				strings.Join(provider.Types(), ", "))
		}
		if a.Source == "keychain" && a.AccessToken != "" {
			return fmt.Errorf("%s: source=keychain and inline accessToken are mutually exclusive", where)
		}
		if a.Upstream != "" {
			if err := validateUpstream(where+" upstream", a.Upstream); err != nil {
				return err
			}
		}
	}
	switch c.Pool.ExhaustedMode {
	case "hold", "fail", "notify":
	default:
		return fmt.Errorf("pool.exhaustedMode: %q must be hold, fail or notify", c.Pool.ExhaustedMode)
	}
	if c.Pool.HoldMax != "" && c.Pool.HoldMax != "0" {
		if _, err := time.ParseDuration(c.Pool.HoldMax); err != nil {
			return fmt.Errorf("pool.holdMax: %q is not a valid duration: %v", c.Pool.HoldMax, err)
		}
	}
	if c.Pool.SwitchThreshold <= 0 || c.Pool.SwitchThreshold > 1 {
		return fmt.Errorf("pool.switchThreshold: %v must be in (0, 1]", c.Pool.SwitchThreshold)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level: %q must be debug, info, warn or error", c.Log.Level)
	}
	if err := validateNotifyChannels(c.Notify.Channels); err != nil {
		return err
	}
	return nil
}

// validateNotifyChannels checks every channel's name, provider and events
// (issue #101). A typo'd event name must fail config load loudly, naming
// the valid set — silently never firing is the worst outcome for a feature
// whose whole job is telling someone something is wrong.
func validateNotifyChannels(channels []NotifyChannelConfig) error {
	seen := map[string]bool{}
	for i, ch := range channels {
		where := fmt.Sprintf("notify.channels[%d]", i)
		if ch.Name != "" {
			where = fmt.Sprintf("notify.channels[%d] (%q)", i, ch.Name)
		}
		if ch.Name == "" {
			return fmt.Errorf("%s: name must not be empty", where)
		}
		if seen[ch.Name] {
			return fmt.Errorf("%s: duplicate channel name", where)
		}
		seen[ch.Name] = true
		if !notify.ProviderKnown(ch.Provider) {
			return fmt.Errorf("%s: provider %q must be one of %s", where, ch.Provider,
				strings.Join(notify.KnownProviders(), ", "))
		}
		for _, e := range ch.Events {
			if !notify.IsValidEvent(e) {
				return fmt.Errorf("%s: event %q must be one of %s", where, e,
					strings.Join(notify.ValidEvents(), ", "))
			}
		}
	}
	return nil
}

func validateUpstream(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s: %q is not a valid absolute URL", field, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme %q must be http or https", field, u.Scheme)
	}
	return nil
}
