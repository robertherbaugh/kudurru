package rpz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/miekg/dns"
)

const maxEnvelopeBytes = 256 << 20

type envelope struct {
	Version     int       `json:"version"`
	ClusterID   string    `json:"clusterID"`
	Identity    string    `json:"identity"`
	Credential  string    `json:"credential"`
	Nonce       string    `json:"nonce"`
	Slot        uint8     `json:"slot"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Primary     string    `json:"primary"`
	Records     []string  `json:"records"`
	Checksum    string    `json:"checksum"`
}

func blobName(identity string) string       { return "rpz/" + identity }
func acceptanceName(identity string) string { return blobName(identity) + "/accepted" }
func candidateName(identity string, slot uint8) string {
	return fmt.Sprintf("%s/candidate-%d", blobName(identity), slot)
}

type acceptance struct {
	Version  int    `json:"version"`
	Slot     uint8  `json:"slot"`
	Checksum string `json:"checksum"`
}

func (e *envelope) checksum() string {
	data := *e
	data.Checksum = ""
	b, _ := json.Marshal(data)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) persist(ctx context.Context, id string, data *envelope) error {
	data.Checksum = data.checksum()
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(b) > maxEnvelopeBytes {
		return fmt.Errorf("rpz: durable snapshot exceeds byte budget")
	}
	if err := m.opts.Blobs.PutBlob(ctx, candidateName(id, data.Slot), b); err != nil {
		return fmt.Errorf("rpz: persisting snapshot: %w", err)
	}
	return nil
}

// accept is a second durable boundary after candidate/configuration checks.
// A failed or raced candidate write leaves a different checksum from the old
// marker and can never be restored, even if blob deletion is unavailable.
func (m *Manager) accept(ctx context.Context, id string, data *envelope) error {
	marker, err := json.Marshal(acceptance{Version: 2, Slot: data.Slot, Checksum: data.Checksum})
	if err != nil {
		return err
	}
	if err := m.opts.Blobs.PutBlob(ctx, acceptanceName(id), marker); err != nil {
		return fmt.Errorf("rpz: accepting durable snapshot: %w", err)
	}
	return nil
}

func (m *Manager) invalidateAcceptance(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	err := m.opts.Blobs.DeleteBlob(ctx, acceptanceName(id))
	if err == nil {
		return nil
	}
	// A deletion failure does not require trusting the raced candidate. A
	// durable empty marker is equally invalid and can replace the old marker.
	if writeErr := m.opts.Blobs.PutBlob(ctx, acceptanceName(id), []byte{}); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return nil
}

func (m *Manager) restore(ctx context.Context, e *entry) (*envelope, *Zone, error) {
	accepted, err := m.opts.Blobs.GetBlob(ctx, acceptanceName(e.spec.Identity))
	if err != nil {
		return nil, nil, err
	}
	var marker acceptance
	if len(accepted.Data) > 1024 || json.Unmarshal(accepted.Data, &marker) != nil || marker.Version != 2 || marker.Slot > 1 || marker.Checksum == "" {
		return nil, nil, fmt.Errorf("rpz: invalid durable acceptance marker")
	}
	blob, err := m.opts.Blobs.GetBlob(ctx, candidateName(e.spec.Identity, marker.Slot))
	if err != nil {
		return nil, nil, err
	}
	if len(blob.Data) > maxEnvelopeBytes {
		return nil, nil, fmt.Errorf("rpz: durable snapshot exceeds byte budget")
	}
	var data envelope
	if err := json.Unmarshal(blob.Data, &data); err != nil {
		return nil, nil, fmt.Errorf("rpz: invalid durable snapshot")
	}
	if data.Version != 2 || data.Nonce == "" || data.ClusterID != m.opts.ClusterID || data.Identity != e.spec.Identity || data.Credential != e.credential || data.Checksum != data.checksum() {
		return nil, nil, fmt.Errorf("rpz: durable version/cluster/identity/credentials/checksum mismatch")
	}
	if marker.Checksum != data.Checksum || marker.Slot != data.Slot {
		return nil, nil, fmt.Errorf("rpz: durable candidate was not accepted")
	}
	if data.ConfirmedAt.IsZero() || data.ConfirmedAt.After(m.opts.Now()) || len(data.Records) == 0 || len(data.Records) > MaxRecords {
		return nil, nil, fmt.Errorf("rpz: invalid durable confirmation or record count")
	}
	var records []dns.RR
	for _, text := range data.Records {
		rr, err := dns.NewRR(text)
		if err != nil {
			return nil, nil, fmt.Errorf("rpz: invalid durable record")
		}
		records = append(records, rr)
	}
	soa, ok := records[0].(*dns.SOA)
	if !ok || !data.ExpiresAt.Equal(data.ConfirmedAt.Add(time.Duration(soa.Expire)*time.Second)) {
		return nil, nil, fmt.Errorf("rpz: invalid durable expiry")
	}
	spec := e.spec
	spec.ExpiresAt = data.ExpiresAt
	z, err := Compile(spec, records)
	if err != nil {
		return nil, nil, fmt.Errorf("rpz: invalid durable policy: %w", err)
	}
	z.confirmedAt = data.ConfirmedAt
	return &data, z, nil
}
