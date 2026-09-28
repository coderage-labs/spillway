package notify

// Event names are a public interface (issue #101): a channel subscribes to
// them by exact string in config, so a typo must fail loudly at config load
// rather than silently never firing — the worst outcome for a feature whose
// whole job is telling someone something is wrong.
const (
	// EventExhausted: the general, account-wide windows have no capacity —
	// every model family is held, not just one. Issue #230: fired once per
	// "all models" episode (open, a material ETA slip, or a single family
	// episode escalating into this one), never once per held/refused
	// request.
	EventExhausted = "exhausted"
	// EventHeld: one model family has no capacity while the general
	// windows (and so every other family) still do — "Fable held until
	// …, other models running". Issue #230: fired once per family
	// episode, same coalescing as EventExhausted above.
	EventHeld = "held"
	// EventOverageCap: an account already serving on paid extra usage has
	// now hit its own limit there too.
	EventOverageCap = "overage-cap"
	// EventAccountDisabled: an account's credential died and it dropped out
	// of rotation (issue #81's class).
	EventAccountDisabled = "account-disabled"
	// EventRecovered: an exhaustion/hold episode's capacity came back —
	// either one model family's own ("Fable back"), the general windows'
	// while a family stayed spent ("Other models back — fable still held
	// until …"), or both together ("All models back"). Issue #230: this is
	// a genuinely new message the owner never received before — the only
	// previous signal that a hold had cleared was the pings simply
	// stopping, which tells nobody who has walked away that it's safe to
	// resume.
	EventRecovered = "recovered"
)

// ValidEvents lists every event a channel may subscribe to. Exported so
// config.Validate can name the valid set in its error message.
func ValidEvents() []string {
	return []string{EventExhausted, EventHeld, EventOverageCap, EventAccountDisabled, EventRecovered}
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
