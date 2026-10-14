package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// StateFormatVersion identifies the bounded complete inventory persistence codec.
const StateFormatVersion = 1

// MaxStateBytes bounds complete encoded state before decoding/publication.
const MaxStateBytes = 256 << 20

// State retains source revisions and winning/removal/ambiguous IP watermarks.
// Restore never refreshes observations or converts tombstones into live claims.
type State struct {
	FormatVersion int           `json:"formatVersion"`
	Generation    uint64        `json:"generation,string"`
	Sources       []SourceState `json:"sources"`
	Watermarks    []Watermark   `json:"watermarks"`
}

// SourceState is the exact last accepted whole source batch, including stale rows.
type SourceState struct {
	ID       string   `json:"id"`
	Revision uint64   `json:"revision,string"`
	Clients  []Client `json:"clients"`
}

// Watermark keeps ownership even when an observation expired or was removed.
// Empty Source/ID is an ambiguous tie. Live distinguishes a claim from removal.
type Watermark struct {
	IP         netip.Addr `json:"ip"`
	Source     string     `json:"source,omitempty"`
	ID         string     `json:"id,omitempty"`
	ObservedAt time.Time  `json:"observedAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	Live       bool       `json:"live"`
}

// ExportState returns sorted isolated complete state under the producer lock.
func (h *Holder) ExportState() (State, error) {
	if h == nil {
		return State{}, errors.New("inventory: nil holder")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := State{FormatVersion: StateFormatVersion}
	for id, b := range h.sources {
		s := SourceState{ID: id, Revision: b.revision, Clients: make([]Client, 0, len(b.clients))}
		for _, c := range b.clients {
			s.Clients = append(s.Clients, clone(*c))
		}
		slices.SortFunc(s.Clients, func(a, b Client) int { return strings.Compare(a.ID, b.ID) })
		out.Sources = append(out.Sources, s)
	}
	slices.SortFunc(out.Sources, func(a, b SourceState) int { return strings.Compare(a.ID, b.ID) })
	if s := h.current.Load(); s != nil {
		out.Generation = s.generation
		for ip, c := range s.byIP {
			out.Watermarks = append(out.Watermarks, Watermark{IP: ip, Source: c.source, ID: c.id, ObservedAt: c.observed, ExpiresAt: c.expires, Live: c.client != nil})
		}
	}
	slices.SortFunc(out.Watermarks, func(a, b Watermark) int { return a.IP.Compare(b.IP) })
	if _, err := EncodeState(out); err != nil {
		return State{}, err
	}
	return out, nil
}

// EncodeState bounds JSON size. RestoreState additionally validates semantics.
func EncodeState(s State) ([]byte, error) {
	if err := CheckStateBounds(s, MaxStateBytes); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > MaxStateBytes {
		return nil, errors.New("inventory: state encoding bound")
	}
	return b, nil
}

// DecodeState rejects oversized/unknown/trailing encoding before semantic restore.
func DecodeState(data []byte) (State, error) {
	if len(data) == 0 || len(data) > MaxStateBytes {
		return State{}, errors.New("inventory: state encoding bound")
	}
	if err := validateStateWire(data); err != nil {
		return State{}, err
	}
	var s State
	if err := decodeStateJSON(data, &s); err != nil {
		return State{}, err
	}
	if _, err := EncodeState(s); err != nil {
		return State{}, err
	}
	return s, nil
}

// RestoreState validates complete evidence without restamping or pruning it.
// Aged rows remain diagnostic only; Lookup enforces their original absolute expiry.
// Replace retains/prunes watermarks using the existing maxAge arbitration.
func RestoreState(s State, now func() time.Time) (*Holder, error) {
	if _, err := EncodeState(s); err != nil {
		return nil, err
	}
	h := New(now)
	at := h.now()
	if at.IsZero() {
		return nil, errors.New("inventory: invalid restore time")
	}
	snap := &Snapshot{generation: s.Generation, byIP: map[netip.Addr]claim{}}
	refs := map[string]map[string]*Client{}
	total := 0
	for _, b := range s.Sources {
		if b.ID == "" || len(b.ID) > 256 || b.Revision == 0 || refs[b.ID] != nil {
			return nil, errors.New("inventory: invalid state source")
		}
		refs[b.ID] = map[string]*Client{}
		clients := make([]*Client, 0, len(b.Clients))
		for _, row := range b.Clients {
			// Validate original lifetime, even for evidence older than the age window.
			if row.Source != b.ID {
				return nil, errors.New("inventory: mismatched state client source")
			}
			c, err := normalize(b.ID, row, row.ObservedAt)
			if err != nil || c.ObservedAt.After(at) || refs[b.ID][c.ID] != nil {
				return nil, errors.New("inventory: invalid state client")
			}
			refs[b.ID][c.ID] = c
			clients = append(clients, c)
			snap.clients = append(snap.clients, c)
			total++
		}
		h.sources[b.ID] = batch{revision: b.Revision, clients: clients}
	}
	if total > maxClients || (len(s.Sources) > 0 && s.Generation == 0) || s.Generation == math.MaxUint64 {
		return nil, errors.New("inventory: invalid state generation/size")
	}
	for _, w := range s.Watermarks {
		if !w.IP.IsValid() || w.IP != w.IP.Unmap() || w.IP.Zone() != "" || w.IP.IsUnspecified() || w.IP.IsMulticast() || w.ObservedAt.IsZero() || w.ObservedAt.After(at) || w.ExpiresAt.Before(w.ObservedAt) || w.ExpiresAt.Sub(w.ObservedAt) > maxAge || (w.Source == "") != (w.ID == "") || len(w.ID) > 256 {
			return nil, errors.New("inventory: invalid watermark")
		}
		if _, exists := snap.byIP[w.IP]; exists {
			return nil, errors.New("inventory: duplicate watermark")
		}
		if w.Source != "" && refs[w.Source] == nil {
			return nil, errors.New("inventory: unknown watermark source")
		}
		p := claim{source: w.Source, id: w.ID, observed: w.ObservedAt, expires: w.ExpiresAt}
		if w.Live {
			c := refs[w.Source][w.ID]
			if c == nil || !slices.Contains(c.IPs, w.IP) || !c.ObservedAt.Equal(w.ObservedAt) || !c.ExpiresAt.Equal(w.ExpiresAt) {
				return nil, errors.New("inventory: invalid live watermark")
			}
			p.client = c
		}
		snap.byIP[w.IP] = p
	}
	// No supplied watermark may suppress a strictly newer current client, lose
	// a required winner, or claim a live winner despite an equal-owner conflict.
	for _, c := range snap.clients {
		for _, ip := range c.IPs {
			p, exists := snap.byIP[ip]
			if !exists || c.ObservedAt.After(p.observed) || c.ObservedAt.Equal(p.observed) && p.client != nil && (p.source != c.Source || p.id != c.ID) {
				return nil, errors.New("inventory: incomplete/inconsistent ownership evidence")
			}
		}
	}
	slices.SortFunc(snap.clients, func(a, b *Client) int {
		if n := strings.Compare(a.Source, b.Source); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	if s.Generation != 0 {
		h.current.Store(snap)
	} else if len(s.Watermarks) > 0 {
		return nil, errors.New("inventory: watermarks without generation")
	}
	return h, nil
}

// PrepareReplacement runs the existing holder arbitration at original admitted
// time. It leaves input unchanged and preserves source revision/removal evidence.
func PrepareReplacement(s State, source string, revision uint64, clients []Client, at time.Time) (State, error) {
	h, err := RestoreState(s, func() time.Time { return at })
	if err != nil {
		return State{}, err
	}
	if err := h.Replace(source, revision, clients); err != nil {
		return State{}, err
	}
	return h.ExportState()
}

func decodeStateJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("inventory: trailing state JSON")
	}
	return nil
}

// CheckStateBounds checks every count/string and exact encoded byte budget
// before allocating a complete JSON payload. It does not validate arbitration.
func CheckStateBounds(s State, maxBytes int) error {
	if maxBytes <= 0 || maxBytes > MaxStateBytes || s.FormatVersion != StateFormatVersion || len(s.Sources) > maxSources || len(s.Watermarks) > maxClients*maxAddresses {
		return errors.New("inventory: invalid state bounds/version")
	}
	size := len(`{"formatVersion":1,"generation":"","sources":null,"watermarks":null}`) + len(strconv.FormatUint(s.Generation, 10))
	// Replace both null placeholders before incremental budget checks. A
	// later empty array must not make an otherwise exact-fit state fail early.
	if s.Sources != nil {
		size += 2 + max(0, len(s.Sources)-1) - 4
	}
	if s.Watermarks != nil {
		size += 2 + max(0, len(s.Watermarks)-1) - 4
	}
	addString := func(value string, limit int) error {
		if len(value) > limit || !utf8.ValidString(value) {
			return errors.New("inventory: invalid state string bound/UTF8")
		}
		size += jsonStringBytes(value)
		if size > maxBytes {
			return errors.New("inventory: state encoding bound")
		}
		return nil
	}
	addTime := func(at time.Time) error {
		b, err := at.MarshalJSON()
		if err != nil {
			return err
		}
		size += len(b)
		return nil
	}
	addIPs := func(ips []netip.Addr) error {
		if ips == nil {
			size += 4
			return nil
		}
		size += 2 + max(0, len(ips)-1)
		for _, ip := range ips {
			b, err := ip.MarshalText()
			if err != nil {
				return err
			}
			if err := addString(string(b), 64); err != nil {
				return err
			}
		}
		return nil
	}
	addStrings := func(items []string, limit int) error {
		size += 2 + max(0, len(items)-1)
		for _, v := range items {
			if err := addString(v, limit); err != nil {
				return err
			}
		}
		return nil
	}
	clients := 0
	if s.Sources != nil {
		for _, source := range s.Sources {
			clients += len(source.Clients)
			if clients > maxClients {
				return errors.New("inventory: state client limit")
			}
			size += len(`{"id":,"revision":"","clients":}`) + len(strconv.FormatUint(source.Revision, 10))
			if err := addString(source.ID, 256); err != nil {
				return err
			}
			if source.Clients == nil {
				size += 4
			} else {
				size += 2 + max(0, len(source.Clients)-1)
				for _, c := range source.Clients {
					if len(c.IPs) > maxAddresses || len(c.MACs) > maxAddresses || len(c.Tags) > 64 {
						return errors.New("inventory: state client slice bound")
					}
					size += len(`{"source":,"id":,"ips":,"observedAt":,"expiresAt":}`)
					if err := addString(c.Source, 256); err != nil {
						return err
					}
					if err := addString(c.ID, 256); err != nil {
						return err
					}
					// Check optional strings even when empty; they serialize only if nonempty.
					if c.Name != "" {
						size += len(`,"name":`)
						if err := addString(c.Name, 253); err != nil {
							return err
						}
					}
					if c.Hostname != "" {
						size += len(`,"hostname":`)
						if err := addString(c.Hostname, 253); err != nil {
							return err
						}
					}
					if err := addIPs(c.IPs); err != nil {
						return err
					}
					if len(c.MACs) > 0 {
						size += len(`,"macs":`)
						if err := addStrings(c.MACs, 17); err != nil {
							return err
						}
					}
					if len(c.Tags) > 0 {
						size += len(`,"tags":`)
						if err := addStrings(c.Tags, 128); err != nil {
							return err
						}
					}
					if err := addTime(c.ObservedAt); err != nil {
						return err
					}
					if err := addTime(c.ExpiresAt); err != nil {
						return err
					}
					if size > maxBytes {
						return errors.New("inventory: state encoding bound")
					}
				}
			}
		}
	}
	if s.Watermarks != nil {
		for _, w := range s.Watermarks {
			size += len(`{"ip":,"observedAt":,"expiresAt":,"live":}`)
			b, err := w.IP.MarshalText()
			if err != nil {
				return err
			}
			if err := addString(string(b), 64); err != nil {
				return err
			}
			if w.Source != "" {
				size += len(`,"source":`)
				if err := addString(w.Source, 256); err != nil {
					return err
				}
			}
			if w.ID != "" {
				size += len(`,"id":`)
				if err := addString(w.ID, 256); err != nil {
					return err
				}
			}
			if err := addTime(w.ObservedAt); err != nil {
				return err
			}
			if err := addTime(w.ExpiresAt); err != nil {
				return err
			}
			if w.Live {
				size += 4
			} else {
				size += 5
			}
			if size > maxBytes {
				return errors.New("inventory: state encoding bound")
			}
		}
	}
	if size > maxBytes {
		return errors.New("inventory: state encoding bound")
	}
	return nil
}
func jsonStringBytes(s string) int {
	n := 2 + len(s)
	for _, r := range s {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			n++
		case '<', '>', '&':
			n += 5
		case '\u2028', '\u2029':
			n += 3
		default:
			if r < 0x20 {
				n += 5
			}
		}
	}
	return n
}

// wireKind constrains each object/array path; no untyped tree is allocated.
type wireKind uint8

const (
	wireState wireKind = iota
	wireSource
	wireClient
	wireWatermark
)

type stateWire struct {
	decoder *json.Decoder
	clients int
}

func validateStateWire(data []byte) error {
	if !validStateUnicode(data) {
		return errors.New("inventory: invalid JSON Unicode")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v := stateWire{decoder: d}
	if err := v.object(wireState); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("inventory: trailing state JSON")
	}
	return nil
}
func (v *stateWire) object(kind wireKind) error {
	token, err := v.decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("inventory: expected state object")
	}
	if kind == wireClient {
		v.clients++
		if v.clients > maxClients {
			return errors.New("inventory: wire client bound")
		}
	}
	seen := map[string]bool{}
	for v.decoder.More() {
		key, err := v.decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("inventory: duplicate state field")
		}
		seen[name] = true
		if err := v.field(kind, name); err != nil {
			return err
		}
	}
	if _, err := v.decoder.Token(); err != nil {
		return err
	}
	var required []string
	switch kind {
	case wireState:
		required = []string{"formatVersion", "generation", "sources", "watermarks"}
	case wireSource:
		required = []string{"id", "revision", "clients"}
	case wireClient:
		required = []string{"source", "id", "ips", "observedAt", "expiresAt"}
	case wireWatermark:
		required = []string{"ip", "observedAt", "expiresAt", "live"}
	}
	for _, key := range required {
		if !seen[key] {
			return errors.New("inventory: missing state field")
		}
	}
	return nil
}
func (v *stateWire) field(kind wireKind, key string) error {
	switch kind {
	case wireState:
		switch key {
		case "formatVersion":
			t, e := v.decoder.Token()
			if e == nil && t == json.Number("1") {
				return nil
			}
			return errors.New("inventory: invalid wire version")
		case "generation":
			return v.integer()
		case "sources":
			return v.objects(wireSource, maxSources)
		case "watermarks":
			return v.objects(wireWatermark, maxClients*maxAddresses)
		}
	case wireSource:
		switch key {
		case "id":
			return v.str(256)
		case "revision":
			return v.integer()
		case "clients":
			return v.objects(wireClient, maxClients)
		}
	case wireClient:
		switch key {
		case "source", "id":
			return v.str(256)
		case "name", "hostname":
			return v.str(253)
		case "observedAt", "expiresAt":
			return v.str(64)
		case "ips":
			return v.strings(maxAddresses, 64, true)
		case "macs":
			return v.strings(maxAddresses, 17, false)
		case "tags":
			return v.strings(64, 128, false)
		}
	case wireWatermark:
		switch key {
		case "source", "id":
			return v.str(256)
		case "ip":
			return v.str(64)
		case "observedAt", "expiresAt":
			return v.str(64)
		case "live":
			t, e := v.decoder.Token()
			if _, ok := t.(bool); e == nil && ok {
				return nil
			}
			return errors.New("inventory: live must be explicit boolean")
		}
	}
	return errors.New("inventory: unknown state field")
}
func (v *stateWire) str(limit int) error {
	t, e := v.decoder.Token()
	s, ok := t.(string)
	if e != nil || !ok || len(s) > limit {
		return errors.New("inventory: invalid state string")
	}
	return nil
}
func (v *stateWire) integer() error {
	t, e := v.decoder.Token()
	s, ok := t.(string)
	if e != nil || !ok || len(s) > 20 {
		return errors.New("inventory: invalid state integer")
	}
	n, e := strconv.ParseUint(s, 10, 64)
	if e != nil || strconv.FormatUint(n, 10) != s {
		return errors.New("inventory: noncanonical state integer")
	}
	return nil
}
func (v *stateWire) objects(kind wireKind, limit int) error {
	t, e := v.decoder.Token()
	if e != nil {
		return e
	}
	if t == nil {
		return nil
	}
	if t != json.Delim('[') {
		return errors.New("inventory: expected state array")
	}
	count := 0
	for v.decoder.More() {
		count++
		if count > limit {
			return errors.New("inventory: wire array bound")
		}
		if e := v.object(kind); e != nil {
			return e
		}
	}
	_, e = v.decoder.Token()
	return e
}
func (v *stateWire) strings(limit, size int, nullable bool) error {
	t, e := v.decoder.Token()
	if e != nil {
		return e
	}
	if t == nil && nullable {
		return nil
	}
	if t != json.Delim('[') {
		return errors.New("inventory: expected string array")
	}
	count := 0
	for v.decoder.More() {
		count++
		if count > limit {
			return errors.New("inventory: wire string array bound")
		}
		if e := v.str(size); e != nil {
			return e
		}
	}
	_, e = v.decoder.Token()
	return e
}
func validStateUnicode(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		i++
		for i < len(data) && data[i] != '"' {
			if data[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			n, e := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if e != nil {
				return false
			}
			i += 5
			if n >= 0xDC00 && n <= 0xDFFF {
				return false
			}
			if n >= 0xD800 && n <= 0xDBFF {
				if i+5 >= len(data) || data[i] != '\\' || data[i+1] != 'u' {
					return false
				}
				low, e := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
				if e != nil || low < 0xDC00 || low > 0xDFFF {
					return false
				}
				i += 6
			}
		}
	}
	return true
}
