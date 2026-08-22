package pool

// Provider capabilities, measured rather than assumed (design doc §6.19).
//
// A request the target cannot serve should never reach it: the upstream error
// is written in the vendor's terms and arrives after the request is spent.
// Preflighting lets the pool route around the mismatch, and lets the failure
// name the actual feature when it cannot.

import (
	"encoding/json"
	"fmt"

	"github.com/coderage-labs/spillway/internal/provider"
)

// ProviderOf maps an account type to its provider family. Rotation and
// capability both key off the family, so two accounts of the same family
// behave alike without either caring which.
func ProviderOf(accountType string) string {
	return string(provider.For(accountType).Kind)
}

// Incompatibility explains why an account cannot serve a request, in terms of
// the feature rather than the vendor's error text.
type Incompatibility struct {
	Account string
	Reason  string
}

func (e *Incompatibility) Error() string {
	return fmt.Sprintf("spillway: account %q cannot serve this request (%s)", e.Account, e.Reason)
}

// requestShape is the subset of a /v1/messages body that capability checks
// care about. Parsed leniently: an unreadable body is not our business here,
// it will fail upstream on its own terms.
type requestShape struct {
	Thinking *struct {
		Type string `json:"type"`
	} `json:"thinking"`
	ToolChoice *struct {
		Type string `json:"type"`
	} `json:"tool_choice"`
}

// CanServe reports whether an account can serve this request body, and why
// not when it cannot. Only positively measured incompatibilities are checked
// — guessing here would route around problems that do not exist.
func CanServe(a *Account, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var shape requestShape
	if json.Unmarshal(body, &shape) != nil {
		return nil
	}
	caps := provider.For(a.Type).Capabilities

	// Thinking is active unless the request explicitly disables it, or the
	// provider needs it explicitly enabled and the request did not.
	thinkingOn := caps.ThinkingDefaultOn
	if shape.Thinking != nil {
		thinkingOn = shape.Thinking.Type == "enabled"
	}

	forcedTool := shape.ToolChoice != nil &&
		(shape.ToolChoice.Type == "tool" || shape.ToolChoice.Type == "function")
	if forcedTool && thinkingOn && !caps.ForcedToolChoiceWithThinking {
		return &Incompatibility{
			Account: a.Name,
			Reason: "a forced tool_choice requires thinking to be disabled on " +
				ProviderOf(a.Type),
		}
	}
	return nil
}
