package dhcp

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// MaxImportBytes bounds native and dnsmasq input before parsing.
const MaxImportBytes = 4 << 20

const maxImportLeases = 65536

var (
	// ErrInvalidImport identifies malformed or unsupported lease input.
	ErrInvalidImport = errors.New("dhcp: invalid lease import")
	// ErrImportTooLarge identifies input exceeding the bounded import limits.
	ErrImportTooLarge = errors.New("dhcp: lease import is too large")
	// ErrUnsupportedExport prevents a lossy dnsmasq export of native state.
	ErrUnsupportedExport = errors.New("dhcp: lease state cannot be represented by dnsmasq")
)

// LeaseIdentity preserves the namespace of a stored client identity. Kind is
// "mac" (canonical MAC value) or "client-id" (base64 of exact stored ID bytes).
type LeaseIdentity struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// LeaseRecord is the lossless native transfer representation of one lease.
type LeaseRecord struct {
	Address   netip.Addr    `json:"address"`
	Identity  LeaseIdentity `json:"identity"`
	MAC       string        `json:"mac"`
	Hostname  string        `json:"hostname"`
	ExpiresAt time.Time     `json:"expiresAt"`
	State     string        `json:"state"`
}

// LeaseDocument is the versioned native JSON export. It transfers runtime
// leases only; reservations and topology stay under scope configuration.
type LeaseDocument struct {
	Version    int           `json:"version"`
	Scope      string        `json:"scope"`
	Subnet     netip.Prefix  `json:"subnet"`
	ExportedAt time.Time     `json:"exportedAt"`
	Leases     []LeaseRecord `json:"leases"`
}

// ImportResult reports a successful atomic merge; failures return no result.
type ImportResult struct {
	Imported       int `json:"imported"`
	Unchanged      int `json:"unchanged"`
	SkippedExpired int `json:"skippedExpired"`
}

// Export takes a consistent live cut, preserving identity, holds and quarantine.
func (s *LeaseStore) Export(ctx context.Context) (LeaseDocument, error) {
	var document LeaseDocument
	err := s.transact(ctx, func(_ *sql.Tx, leases []storedLease, now time.Time) error {
		document = LeaseDocument{Version: 1, Scope: s.cfg.Name, Subnet: s.cfg.Subnet,
			ExportedAt: now, Leases: make([]LeaseRecord, 0, len(leases))}
		for _, l := range leases {
			identity := LeaseIdentity{Kind: "mac", Value: l.MAC}
			if strings.HasPrefix(l.key, "id:") {
				identity = LeaseIdentity{Kind: "client-id", Value: base64.StdEncoding.EncodeToString([]byte(l.ClientID))}
			}
			document.Leases = append(document.Leases, LeaseRecord{Address: l.Address, Identity: identity,
				MAC: l.MAC, Hostname: l.Hostname, ExpiresAt: l.ExpiresAt, State: l.State})
		}
		return nil
	})
	if err != nil {
		return LeaseDocument{}, err
	}
	return document, nil
}

func decodeNative(data []byte) (LeaseDocument, error) {
	var document LeaseDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return LeaseDocument{}, fmt.Errorf("%w: %v", ErrInvalidImport, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return LeaseDocument{}, fmt.Errorf("%w: expected one native document", ErrInvalidImport)
	}
	if document.Version != 1 || document.Scope == "" || !document.Subnet.IsValid() || !document.Subnet.Addr().Is4() || document.Subnet != document.Subnet.Masked() {
		return LeaseDocument{}, fmt.Errorf("%w: unsupported document version or scope/subnet", ErrInvalidImport)
	}
	if len(document.Leases) > maxImportLeases {
		return LeaseDocument{}, ErrImportTooLarge
	}
	return document, nil
}

func importedLease(record LeaseRecord) (storedLease, error) {
	mac, err := canonicalMAC(record.MAC)
	if err != nil || !record.Address.Is4() || !validHostname(record.Hostname) || record.ExpiresAt.Unix() <= 0 || record.ExpiresAt.Year() > 9999 || record.ExpiresAt.Nanosecond() != 0 {
		return storedLease{}, fmt.Errorf("%w: invalid MAC, address, hostname or absolute expiry", ErrInvalidImport)
	}
	if record.State != "bound" && record.State != "offered" && record.State != "declined" {
		return storedLease{}, fmt.Errorf("%w: unknown lease state", ErrInvalidImport)
	}
	l := storedLease{Lease: Lease{Address: record.Address, MAC: mac, Hostname: record.Hostname,
		ExpiresAt: record.ExpiresAt.UTC(), State: record.State}}
	switch record.Identity.Kind {
	case "mac":
		identityMAC, err := canonicalMAC(record.Identity.Value)
		if err != nil || identityMAC != mac {
			return storedLease{}, fmt.Errorf("%w: MAC identity does not match hardware address", ErrInvalidImport)
		}
		l.ClientID, l.key = mac, "mac:"+mac
	case "client-id":
		value, err := base64.StdEncoding.Strict().DecodeString(record.Identity.Value)
		if err != nil || len(value) == 0 || len(value) > 1024 {
			return storedLease{}, fmt.Errorf("%w: malformed opaque client identity", ErrInvalidImport)
		}
		l.ClientID, l.key = string(value), "id:"+string(value)
	default:
		return storedLease{}, fmt.Errorf("%w: unknown client identity kind", ErrInvalidImport)
	}
	return l, nil
}

func decodeDNSMasq(data []byte) ([]LeaseRecord, error) {
	var records []LeaseRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4096)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 5 {
			return nil, fmt.Errorf("%w: dnsmasq line %d must have five fields", ErrInvalidImport, line)
		}
		expires, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || expires <= 0 {
			return nil, fmt.Errorf("%w: dnsmasq line %d has invalid or unsupported infinite expiry", ErrInvalidImport, line)
		}
		mac, err := canonicalMAC(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%w: dnsmasq line %d has invalid Ethernet MAC", ErrInvalidImport, line)
		}
		address, err := netip.ParseAddr(fields[2])
		if err != nil || !address.Is4() {
			return nil, fmt.Errorf("%w: dnsmasq line %d must contain IPv4", ErrInvalidImport, line)
		}
		hostname := fields[3]
		if hostname == "*" {
			hostname = ""
		}
		identity := LeaseIdentity{Kind: "mac", Value: mac}
		if fields[4] != "*" {
			parts := strings.Split(fields[4], ":")
			if len(parts) < 2 || len(parts) > 255 {
				return nil, fmt.Errorf("%w: dnsmasq line %d has invalid client ID length", ErrInvalidImport, line)
			}
			for _, part := range parts {
				if len(part) != 2 {
					return nil, fmt.Errorf("%w: dnsmasq line %d has malformed client ID", ErrInvalidImport, line)
				}
			}
			id := strings.Join(parts, "")
			if _, err := hex.DecodeString(id); err != nil {
				return nil, fmt.Errorf("%w: dnsmasq line %d has malformed client ID", ErrInvalidImport, line)
			}
			// Wire clients use lower-case hex of the exact option 61 bytes.
			identity = LeaseIdentity{Kind: "client-id", Value: base64.StdEncoding.EncodeToString([]byte(strings.ToLower(id)))}
		}
		records = append(records, LeaseRecord{Address: address, Identity: identity, MAC: mac,
			Hostname: hostname, ExpiresAt: time.Unix(expires, 0).UTC(), State: "bound"})
		if len(records) > maxImportLeases {
			return nil, ErrImportTooLarge
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: invalid or oversized dnsmasq line", ErrInvalidImport)
	}
	return records, nil
}

// Import atomically merges bounded native JSON or dnsmasq leases. It is also
// available in file mode because it never changes configuration or topology.
func (s *LeaseStore) Import(ctx context.Context, format string, data []byte) (ImportResult, error) {
	if len(data) > MaxImportBytes {
		return ImportResult{}, ErrImportTooLarge
	}
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	var records []LeaseRecord
	var document LeaseDocument
	var err error
	switch format {
	case "native":
		document, err = decodeNative(data)
		records = document.Leases
	case "dnsmasq":
		records, err = decodeDNSMasq(data)
	default:
		err = fmt.Errorf("%w: format must be native or dnsmasq", ErrInvalidImport)
	}
	if err != nil {
		return ImportResult{}, err
	}
	var result ImportResult
	err = s.transact(ctx, func(tx *sql.Tx, current []storedLease, now time.Time) error {
		if format == "native" && (document.Scope != s.cfg.Name || document.Subnet != s.cfg.Subnet) {
			return fmt.Errorf("%w: native scope/subnet does not match target", ErrInvalidImport)
		}
		byAddress := make(map[netip.Addr]storedLease, len(current))
		byOwner := make(map[string]netip.Addr, len(current))
		for _, l := range current {
			byAddress[l.Address] = l
			if l.State != "declined" {
				byOwner[l.key] = l.Address
			}
		}
		seenAddress, seenOwner := map[netip.Addr]bool{}, map[string]bool{}
		var planned []storedLease
		for i, record := range records {
			if err := ctx.Err(); err != nil {
				return err
			}
			l, err := importedLease(record)
			if err != nil {
				return fmt.Errorf("record %d: %w", i+1, err)
			}
			if !now.Before(l.ExpiresAt) {
				result.SkippedExpired++
				continue
			}
			if seenAddress[l.Address] || l.State != "declined" && seenOwner[l.key] {
				return fmt.Errorf("%w: duplicate address or client ownership in input", ErrInvalidImport)
			}
			seenAddress[l.Address] = true
			if l.State != "declined" {
				seenOwner[l.key] = true
			}
			if err := s.compatible(l); err != nil {
				return fmt.Errorf("%w: imported address %s conflicts with configuration", ErrUnavailable, l.Address)
			}
			if occupied, found := byOwner[l.key]; l.State != "declined" && found && occupied != l.Address {
				return fmt.Errorf("%w: client already holds another address", ErrUnavailable)
			}
			if old, found := byAddress[l.Address]; found {
				if old.key != l.key || old.MAC != l.MAC || (old.State == "declined") != (l.State == "declined") {
					return fmt.Errorf("%w: imported address already has different ownership or quarantine", ErrUnavailable)
				}
				// Merging a backup does not rename a live client or downgrade
				// its binding. Existing expiration is always a lower bound.
				l.Hostname = old.Hostname
				if old.State == "bound" {
					l.State = "bound"
				}
				if old.ExpiresAt.After(l.ExpiresAt) {
					l.ExpiresAt = old.ExpiresAt
				}
				if old == l {
					result.Unchanged++
					continue
				}
			}
			planned = append(planned, l)
			result.Imported++
		}
		for _, l := range planned {
			if err := saveLease(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

// ExportDNSMasq exports bound IPv4 leases in dnsmasq's five-column format.
// Holds, quarantine and non-wire identities fail rather than silently lose data.
func (s *LeaseStore) ExportDNSMasq(ctx context.Context) ([]byte, error) {
	var output bytes.Buffer
	err := s.transact(ctx, func(_ *sql.Tx, leases []storedLease, _ time.Time) error {
		for _, l := range leases {
			if l.State != "bound" {
				return ErrUnsupportedExport
			}
			hostname, id := l.Hostname, "*"
			if hostname == "" {
				hostname = "*"
			}
			if strings.HasPrefix(l.key, "id:") {
				value, err := hex.DecodeString(l.ClientID)
				if err != nil || len(value) < 2 || len(value) > 255 {
					return ErrUnsupportedExport
				}
				parts := make([]string, len(value))
				for i, b := range value {
					parts[i] = fmt.Sprintf("%02x", b)
				}
				id = strings.Join(parts, ":")
			}
			fmt.Fprintf(&output, "%d %s %s %s %s\n", l.ExpiresAt.Unix(), l.MAC, l.Address, hostname, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
