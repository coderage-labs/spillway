package notify

import (
	"fmt"
	"log/slog"
)

// ChannelSpec is one configured destination's metadata: name, provider,
// subscribed events. This is everything the config file holds (§5) — the
// credential comes from CredentialSource, never from a ChannelSpec.
type ChannelSpec struct {
	Name     string
	Provider string
	Events   []string
}

// CredentialSource resolves one channel's secret Destination — typically a
// secrets.Store.GetRaw(ChannelKey(name)) read, unmarshalled. A function type
// rather than an interface so this package never needs to import
// internal/secrets.
type CredentialSource func(channelName string) (Destination, error)

// ChannelKey returns the secret-store key for a notify channel's
// credential, mirroring internal/secrets' "account:<name>" convention for
// accounts (issue #101: config holds metadata only, keyed "notify:<name>").
func ChannelKey(name string) string { return "notify:" + name }

// channel is one resolved, ready-to-use destination.
type channel struct {
	name     string
	provider Provider
	events   map[string]bool
	dest     Destination
	// enabled is false when the credential is missing or failed to load —
	// kept in the list (so `notify list` can still show it) rather than
	// dropped, and skipped at send time with the warning already issued at
	// load time.
	enabled bool
}

// SetChannels resolves specs into the Notifier's channel list, replacing
// whatever was there before. Each spec's credential is resolved via creds;
// a failure there (missing entry, or a broken keychain read — issues #65,
// #81) disables that one channel with a warning rather than the whole
// daemon failing to start. Returns one warning string per problem found,
// for the caller to log.
func (n *Notifier) SetChannels(specs []ChannelSpec, creds CredentialSource) []string {
	var warnings []string
	chans := make([]channel, 0, len(specs))
	for _, spec := range specs {
		events := map[string]bool{}
		for _, e := range spec.Events {
			events[e] = true
		}

		if spec.Provider == "os" {
			// No secret store round trip: "os" needs no credential at all,
			// so a missing one is not a failure worth warning about.
			p, enabled, _ := providerFor(spec.Provider, n.send, n.Enabled)
			chans = append(chans, channel{name: spec.Name, provider: p, events: events, enabled: enabled})
			continue
		}

		p, _, ok := providerFor(spec.Provider, n.send, n.Enabled)
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"notify channel %q: unknown provider %q — this channel is disabled", spec.Name, spec.Provider))
			chans = append(chans, channel{name: spec.Name, events: events, enabled: false})
			continue
		}
		dest, err := creds(spec.Name)
		if err != nil || dest.Empty() {
			warnings = append(warnings, fmt.Sprintf(
				"notify channel %q has no credential — it will not fire until you run `spillway notify set %s`",
				spec.Name, spec.Name))
			chans = append(chans, channel{name: spec.Name, provider: p, events: events, enabled: false})
			continue
		}
		chans = append(chans, channel{name: spec.Name, provider: p, events: events, dest: dest, enabled: true})
	}

	n.mu.Lock()
	n.channels = chans
	n.mu.Unlock()
	return warnings
}

// SetLogger wires where delivery failures are reported. nil-safe to leave
// unset (tests, and callers that don't care).
func (n *Notifier) SetLogger(logger *slog.Logger) { n.logger = logger }

func (n *Notifier) log() *slog.Logger {
	if n.logger != nil {
		return n.logger
	}
	return slog.Default()
}
