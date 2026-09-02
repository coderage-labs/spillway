package notify

// Event names are a public interface (issue #101): a channel subscribes to
// them by exact string in config, so a typo must fail loudly at config load
// rather than silently never firing — the worst outcome for a feature whose
// whole job is telling someone something is wrong.
const (
	// EventExhausted: every account is spent and the request was refused.
	EventExhausted = "exhausted"
	// EventHeld: the first request has been parked waiting for a reset —
	// the leading indicator, before a queue builds.
	EventHeld = "held"
	// EventOverageCap: an account already serving on paid extra usage has
	// now hit its own limit there too.
	EventOverageCap = "overage-cap"
	// EventAccountDisabled: an account's credential died and it dropped out
	// of rotation (issue #81's class).
	EventAccountDisabled = "account-disabled"
)

// ValidEvents lists every event a channel may subscribe to. Exported so
// config.Validate can name the valid set in its error message.
func ValidEvents() []string {
	return []string{EventExhausted, EventHeld, EventOverageCap, EventAccountDisabled}
}

// IsValidEvent reports whether s is a known event name.
func IsValidEvent(s string) bool {
	for _, e := range ValidEvents() {
		if e == s {
			return true
		}
	}
	return false
}

// KnownProviders lists every provider a channel may name. Deliberately not a
// plugin system (issue #101) — a small registry keyed by this string is the
// right size.
func KnownProviders() []string {
	return []string{"os", "webhook", "ntfy", "pushover"}
}

// ProviderKnown reports whether s is a registered provider name.
func ProviderKnown(s string) bool {
	for _, p := range KnownProviders() {
		if p == s {
			return true
		}
	}
	return false
}
