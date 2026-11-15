package rpz

import (
	"errors"
	"slices"
	"strings"
)

// MaxIgnoredExamples bounds the owner samples reported per ignored reason.
const MaxIgnoredExamples = 5

// RuleCounts counts compiled policy owners by trigger and by action. Every
// compiled owner has exactly one trigger and one action.
type RuleCounts struct {
	QName      int `json:"qname"`
	ClientIP   int `json:"clientIP"`
	ResponseIP int `json:"responseIP"`
	NXDOMAIN   int `json:"nxdomain"`
	NODATA     int `json:"nodata"`
	Passthru   int `json:"passthru"`
	Drop       int `json:"drop"`
	TCPOnly    int `json:"tcpOnly"`
	LocalData  int `json:"localData"`
}

func (c *RuleCounts) add(e Entry) {
	switch e.Trigger {
	case TriggerQName:
		c.QName++
	case TriggerClientIP:
		c.ClientIP++
	case TriggerResponseIP:
		c.ResponseIP++
	}
	switch e.Action {
	case ActionNXDOMAIN:
		c.NXDOMAIN++
	case ActionNODATA:
		c.NODATA++
	case ActionPassthru:
		c.Passthru++
	case ActionDrop:
		c.Drop++
	case ActionTCPOnly:
		c.TCPOnly++
	case ActionLocalData:
		c.LocalData++
	}
}

// IgnoredReason accounts for every policy owner the compiler ignored for one
// reason (D391). Count is complete; Examples holds at most MaxIgnoredExamples
// owner names from the bounded diagnostic samples. Unsupported is true for a
// well-formed policy kudurru does not implement (NSDNAME, NSIP, DNAME, an
// unknown rpz- trigger or action, a non-terminal wildcard) and false for a
// malformed owner (an invalid address trigger or CNAME target).
type IgnoredReason struct {
	Reason      string   `json:"reason"`
	Unsupported bool     `json:"unsupported"`
	Count       int      `json:"count"`
	Examples    []string `json:"examples,omitempty"`
}

// Rules returns the compiled owner counts by trigger and action.
func (z *Zone) Rules() RuleCounts {
	if z == nil {
		return RuleCounts{}
	}
	return z.ruleCounts
}

// Ignored returns the complete per-reason accounting of ignored owners, in
// reason order. The result is a private copy.
func (z *Zone) Ignored() []IgnoredReason {
	if z == nil || len(z.reasons) == 0 {
		return nil
	}
	out := make([]IgnoredReason, 0, len(z.reasons))
	for reason, count := range z.reasons {
		item := IgnoredReason{Reason: reason, Count: count}
		for _, d := range z.diagnostics {
			if d.Reason != reason {
				continue
			}
			item.Unsupported = d.Unsupported
			if len(item.Examples) < MaxIgnoredExamples {
				item.Examples = append(item.Examples, d.Owner)
			}
		}
		if len(item.Examples) == 0 {
			item.Unsupported = unsupportedReason(reason)
		}
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b IgnoredReason) int { return strings.Compare(a.Reason, b.Reason) })
	return out
}

// unsupportedReason classifies a reason whose samples were all dropped by the
// diagnostic cap. Only malformed owners are not "unsupported".
func unsupportedReason(reason string) bool {
	return reason != "invalid-address-trigger" && reason != "invalid-cname-action"
}

// ErrListNotConfigured reports a BlockList absent from the node's current
// configuration snapshot.
var ErrListNotConfigured = errors.New("rpz: list is not in this node's configuration")

// ErrConfigurationUnavailable reports a node without any configuration snapshot.
var ErrConfigurationUnavailable = errors.New("rpz: this node has no configuration snapshot")
