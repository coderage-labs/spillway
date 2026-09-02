package main

// `spillway notify set|test|list|remove` (issue #101): channel management,
// mirroring `spillway login`/`accounts`'s shape. Config holds only
// name/provider/events (§5); the credential always goes to the secret
// store under notify.ChannelKey(name), the same way `login` never writes a
// token into the yaml.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func runNotify(args []string) error {
	if len(args) == 0 {
		return notifyUsageErr()
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: spillway notify set <channel>")
		}
		return runNotifySet(args[1])
	case "test":
		if len(args) < 2 {
			return fmt.Errorf("usage: spillway notify test <channel>")
		}
		return runNotifyTest(args[1])
	case "list":
		return runNotifyList()
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: spillway notify remove <channel>")
		}
		return runNotifyRemove(args[1])
	default:
		return notifyUsageErr()
	}
}

func notifyUsageErr() error {
	return fmt.Errorf("usage: spillway notify set|test|list|remove <channel>")
}

// buildNotifier resolves cfg's channels (if any) into a live Notifier.
// Empty channels means today's exact local-only behaviour (issue #101: off
// by default) — notify.New() alone, untouched. Returns warnings for the
// caller to log: an unknown provider, or a channel with no credential yet
// (a broken keychain read included — issues #65, #81 — a failed read here
// must disable that one channel, never the daemon).
func buildNotifier(cfg *config.Config, store secrets.Store) (*notify.Notifier, []string) {
	n := notify.New()
	return n, setNotifyChannels(n, cfg, store)
}

// setNotifyChannels resolves cfg's channels onto an existing Notifier,
// replacing whatever was there. Split out of buildNotifier for issue #84:
// channels used to be read exactly once, at startup, so `spillway notify
// set phone` wrote the config and the credential and then did nothing at
// all until the daemon was restarted — the concrete case that motivated
// watching the config file. Every reload path now goes through here.
//
// No channels calls SetChannels with an empty list rather than skipping it:
// skipping was equivalent at startup (an unset channel list and an empty
// one both mean "local desktop notification only"), but on a reload the
// difference is whether removing the last channel actually stops it firing.
//
// Nothing secret passes through cfg: the config holds a channel's name,
// provider and events (§5), and the credential comes from the secret store
// via channelCredentialSource. The warnings returned name channels only.
func setNotifyChannels(n *notify.Notifier, cfg *config.Config, store secrets.Store) []string {
	specs := make([]notify.ChannelSpec, len(cfg.Notify.Channels))
	for i, c := range cfg.Notify.Channels {
		specs[i] = notify.ChannelSpec{Name: c.Name, Provider: c.Provider, Events: c.Events}
	}
	return n.SetChannels(specs, channelCredentialSource(store))
}

// channelCredentialSource adapts the secret store to notify.CredentialSource.
func channelCredentialSource(store secrets.Store) notify.CredentialSource {
	return func(name string) (notify.Destination, error) {
		raw, err := store.GetRaw(notify.ChannelKey(name))
		if err != nil {
			return notify.Destination{}, err
		}
		var d notify.Destination
		if err := json.Unmarshal(raw, &d); err != nil {
			return notify.Destination{}, fmt.Errorf("channel %q credential malformed: %w", name, err)
		}
		return d, nil
	}
}

// runNotifySet implements `spillway notify set <channel>`: prompts for a
// provider, its credential (none for "os"), and events; writes the
// credential to the secret store and the metadata to the config.
func runNotifySet(name string) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	existing, _, err := config.FindNotifyChannel(cfgPath, name)
	if err != nil {
		return err
	}
	existing.Name = name

	spec, dest, err := promptNotifyChannel(bufio.NewReader(os.Stdin), os.Stdout, name, existing)
	if err != nil {
		return err
	}

	store := openSecrets()
	if spec.Provider == "os" {
		// No credential to store; drop any stale one left by a previous
		// provider on this same channel name.
		if err := store.DeleteRaw(notify.ChannelKey(name)); err != nil {
			return err
		}
	} else {
		blob, err := json.Marshal(dest)
		if err != nil {
			return err
		}
		if err := store.SetRaw(notify.ChannelKey(name), blob); err != nil {
			return err
		}
	}
	if err := config.UpsertNotifyChannel(cfgPath, spec); err != nil {
		return err
	}
	events := "all"
	if len(spec.Events) > 0 {
		events = strings.Join(spec.Events, ",")
	}
	fmt.Printf("notify channel %q set (provider %s, events %s)\n", name, spec.Provider, events)
	// Issue #84's motivating report: this wrote the config and the
	// credential and then stopped, so the channel did nothing at all until
	// the daemon was restarted — for a feature whose whole job is reaching
	// someone who is not at the machine.
	fmt.Println(liveApplyConfigEdit())
	fmt.Println("run `spillway notify test", name, "` to send a real notification through it")
	return nil
}

// promptNotifyChannel drives the interactive prompts. Split out from
// runNotifySet so it's testable with a fake reader/writer instead of real
// stdin/stdout.
func promptNotifyChannel(r *bufio.Reader, w io.Writer, name string, existing config.NotifyChannelConfig) (config.NotifyChannelConfig, notify.Destination, error) {
	provider, err := promptRequired(r, w,
		fmt.Sprintf("provider (%s) [%s]: ", strings.Join(notify.KnownProviders(), "/"), existing.Provider),
		existing.Provider)
	if err != nil {
		return config.NotifyChannelConfig{}, notify.Destination{}, err
	}
	if !notify.ProviderKnown(provider) {
		return config.NotifyChannelConfig{}, notify.Destination{}, fmt.Errorf(
			"provider must be one of %s (got %q)", strings.Join(notify.KnownProviders(), ", "), provider)
	}

	defEvents := strings.Join(existing.Events, ",")
	if defEvents == "" {
		defEvents = strings.Join(notify.ValidEvents(), ",")
	}
	eventsLine, err := promptLine(r, w,
		fmt.Sprintf("events (%s) [%s]: ", strings.Join(notify.ValidEvents(), ","), defEvents), defEvents)
	if err != nil {
		return config.NotifyChannelConfig{}, notify.Destination{}, err
	}
	var events []string
	for _, e := range strings.Split(eventsLine, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !notify.IsValidEvent(e) {
			return config.NotifyChannelConfig{}, notify.Destination{}, fmt.Errorf(
				"event %q must be one of %s", e, strings.Join(notify.ValidEvents(), ", "))
		}
		events = append(events, e)
	}

	var dest notify.Destination
	switch provider {
	case "webhook":
		u, err := promptRequired(r, w, "webhook URL: ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.URL = u
		tok, err := promptLine(r, w, "bearer token (blank for none): ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.Token = tok
	case "ntfy":
		// Issue #101 comment: on ntfy.sh there is NO access control at
		// all — the topic itself is the entire credential, both to read
		// AND to publish fake alerts. A guessable topic is not a weaker
		// version of this feature, it is a different, insecure one.
		topic, err := promptRequired(r, w,
			"ntfy topic (generate with `openssl rand -hex 16`, never a memorable name): ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.Topic = topic
		base, err := promptLine(r, w, "self-hosted base URL (blank for ntfy.sh): ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.URL = base
		tok, err := promptLine(r, w,
			"access token tk_... (blank for none — needed for a reserved or self-hosted topic): ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.Token = tok
	case "pushover":
		tok, err := promptRequired(r, w, "pushover app token: ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.Token = tok
		uk, err := promptRequired(r, w, "pushover user key: ", "")
		if err != nil {
			return config.NotifyChannelConfig{}, notify.Destination{}, err
		}
		dest.UserKey = uk
	case "os":
		// No credential — the platform-local notifier needs none.
	}

	return config.NotifyChannelConfig{Name: name, Provider: provider, Events: events}, dest, nil
}

func promptLine(r *bufio.Reader, w io.Writer, prompt, def string) (string, error) {
	fmt.Fprint(w, prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def, nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptRequired(r *bufio.Reader, w io.Writer, prompt, def string) (string, error) {
	v, err := promptLine(r, w, prompt, def)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("a value is required")
	}
	return v, nil
}

// runNotifyTest implements `spillway notify test <channel>`: an honest,
// synchronous send — bypassing Notify's dedup and fire-and-forget delivery
// entirely — because a push channel that silently does not work is
// indistinguishable from a quiet system, and this command exists so it
// doesn't have to be.
func runNotifyTest(name string) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	spec, ok, err := config.FindNotifyChannel(cfgPath, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("notify channel %q not found — run `spillway notify set %s`", name, name)
	}

	var dest notify.Destination
	if spec.Provider != "os" {
		store := openSecrets()
		raw, err := store.GetRaw(notify.ChannelKey(name))
		if err != nil {
			return fmt.Errorf("channel %q has no credential — run `spillway notify set %s` (%v)", name, name, err)
		}
		if err := json.Unmarshal(raw, &dest); err != nil {
			return fmt.Errorf("channel %q credential malformed: %w", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = notify.Send(ctx, spec.Provider, dest, notify.Notification{
		Event: "test",
		Key:   "manual-test",
		Title: "spillway: test notification",
		Body:  "If you can see this, " + name + " is wired up correctly.",
	})
	if err != nil {
		return fmt.Errorf("channel %q: send failed: %w", name, err)
	}
	fmt.Printf("channel %q: sent\n", name)
	return nil
}

// runNotifyList implements `spillway notify list`.
func runNotifyList() error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	channels, err := config.ListNotifyChannels(cfgPath)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		fmt.Println("no notify channels — local desktop notifications only; run `spillway notify set <name>` to add one")
		return nil
	}
	store := openSecrets()
	t := newTable("channel", "provider", "events", "credential")
	for _, c := range channels {
		cred := "n/a"
		if c.Provider != "os" {
			if _, err := store.GetRaw(notify.ChannelKey(c.Name)); err == nil {
				cred = "present"
			} else {
				cred = "missing"
			}
		}
		events := "all"
		if len(c.Events) > 0 {
			events = strings.Join(c.Events, ",")
		}
		t.add(c.Name, c.Provider, events, cred)
	}
	t.render(os.Stdout)
	return nil
}

// runNotifyRemove implements `spillway notify remove <channel>`: deletes the
// config entry and its secret, mirroring `accounts remove`.
func runNotifyRemove(name string) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	if err := config.RemoveNotifyChannel(cfgPath, name); err != nil {
		return err
	}
	store := openSecrets()
	if err := store.DeleteRaw(notify.ChannelKey(name)); err != nil {
		return err
	}
	fmt.Println("removed:", name)
	// Symmetrical with `notify set`: a channel removed from the config must
	// stop firing on the running daemon, not at the next restart.
	fmt.Println(liveApplyConfigEdit())
	return nil
}
