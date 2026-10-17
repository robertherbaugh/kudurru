package dhcpsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Once quorum identity exists, local setup must never rewrite shared ownership
// policy. Live options/reservations use the replicated management API instead.
// Local interface and credential changes remain ordinary staged setup edits.
func (m *Manager) validateEstablishedFailover(next config.DHCPFailover, _ bool, scopes []config.DHCPScope, current State) error {
	_, err := os.Stat(filepath.Join(filepath.Dir(m.path), "dhcp-raft", "identity.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !next.Enabled {
		return fmt.Errorf("%w: retain established failover settings; disable DHCP to stop this node without creating an independent allocator", ErrInvalidConfiguration)
	}
	before, after := current.Failover.Clone(), next.Clone()
	// These node-local values do not change the consensus identity.
	before.CAFile, before.CertFile, before.KeyFile, before.ListenAddress = "", "", "", ""
	after.CAFile, after.CertFile, after.KeyFile, after.ListenAddress = "", "", "", ""
	before.Bootstrap, after.Bootstrap = false, false
	slices.SortFunc(before.Peers, func(a, b config.DHCPFailoverPeer) int { return compareString(a.ID, b.ID) })
	slices.SortFunc(after.Peers, func(a, b config.DHCPFailoverPeer) int { return compareString(a.ID, b.ID) })
	for i := range before.Peers {
		before.Peers[i].APIURL = ""
	}
	for i := range after.Peers {
		after.Peers[i].APIURL = ""
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("%w: failover cluster ID, node ID, and voters are fixed after initialization; restore the established membership", ErrInvalidConfiguration)
	}
	if len(scopes) != len(current.Scopes) {
		return fmt.Errorf("%w: established failover scope registry is fixed; adding or removing scopes requires a coordinated cluster migration", ErrInvalidConfiguration)
	}
	byName := map[string]config.DHCPScope{}
	for _, scope := range current.Scopes {
		byName[scope.Name] = scope
	}
	for _, desired := range scopes {
		old, ok := byName[desired.Name]
		if !ok {
			return fmt.Errorf("%w: established failover scopes cannot be renamed", ErrInvalidConfiguration)
		}
		old.Interface, desired.Interface = "", ""
		if !reflect.DeepEqual(old, desired) {
			return fmt.Errorf("%w: scope %s belongs to an established failover cluster; use its live network options and reservations editors for replicated changes; shared topology requires a coordinated cluster migration", ErrInvalidConfiguration, desired.Name)
		}
	}
	return nil
}

func compareString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
