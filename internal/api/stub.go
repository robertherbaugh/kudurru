package api

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/stub"
)

// StubAuthority is a discovered, pre-resolved authority on this node.
type StubAuthority struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Protocol string `json:"protocol" enum:"udp,tcp"`
}

// StubRuntime reports local routing availability, not possession of a zone.
type StubRuntime struct {
	Node            string          `json:"node"`
	State           string          `json:"state" enum:"pending,routing,expired,unavailable"`
	Serial          *uint32         `json:"serial,omitempty" format:"int64" minimum:"0" maximum:"4294967295"`
	Authorities     []StubAuthority `json:"authorities" nullable:"false"`
	LastConfirmedAt *time.Time      `json:"lastConfirmedAt,omitempty"`
	ExpiresAt       *time.Time      `json:"expiresAt,omitempty"`
	NextAttemptAt   *time.Time      `json:"nextAttemptAt,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Message         string          `json:"message,omitempty"`
}

func stubRuntime(node string, st stub.Status, found bool) *StubRuntime {
	out := &StubRuntime{Node: node, State: "pending", Authorities: []StubAuthority{}, Reason: "DiscoveryPending", Message: "Waiting for this node to discover the zone's authorities."}
	if !found {
		return out
	}
	if !st.NextRefresh.IsZero() {
		out.NextAttemptAt = &st.NextRefresh
	}
	if !st.ConfirmedAt.IsZero() {
		out.Serial = &st.Serial
		out.LastConfirmedAt, out.ExpiresAt = &st.ConfirmedAt, &st.ExpiresAt
		for _, authority := range st.Servers {
			out.Authorities = append(out.Authorities, StubAuthority{Name: authority.Name, Address: authority.Address.String(), Protocol: authority.Protocol})
		}
		out.State, out.Reason, out.Message = "expired", "StubExpired", "The discovered route has expired; this node answers SERVFAIL for this zone."
		if st.Available {
			out.State, out.Reason, out.Message = "routing", "", ""
		}
	}
	if st.Error != "" {
		if out.State == "pending" {
			out.State = "unavailable"
		}
		out.Reason, out.Message = "StubDiscovery", st.Error
	}
	if st.ConfigurationError != "" {
		out.State, out.Reason, out.Message = "unavailable", "ConfigurationUnavailable", st.ConfigurationError
	}
	return out
}
