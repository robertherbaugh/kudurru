package catalogue

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxSourceBytes = 8192

// cacheReceipt keeps original body bytes and their actual source in one atomic
// file. The old independent document/source pair cannot prove that association
// after a crash, so it is deliberately not used to initialize a captured owner.
type cacheReceipt struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Digest  string `json:"digest"`
	Body    []byte `json:"body"`
}

func (r *Refresher) receiptName() string {
	if r.opts.Kind == KindAdGuard {
		return fileImportReceipt
	}
	return fileReceipt
}

func (r *Refresher) receiptLimit() int64 {
	// Body is base64; a JSON-escaped bounded source can expand sixfold.
	return int64(4*((r.maxBytes()+2)/3) + 6*maxSourceBytes + 256)
}

// persist runs outside the holder installation gate. Rename never exposes a
// body paired with another source, even if writing or syncing is interrupted.
func (r *Refresher) persist(raw []byte, origin string) {
	if r.opts.Dir == "" || origin == OriginEmbedded {
		return
	}
	if err := r.saveReceipt(raw, origin); err != nil {
		r.log.Warn("catalog: cannot keep the download receipt", "err", err)
	}
}

func (r *Refresher) saveReceipt(raw []byte, origin string) error {
	if origin != r.opts.URL || len(origin) > maxSourceBytes || len(raw) == 0 || len(raw) > r.maxBytes() {
		return errors.New("catalogue: invalid cache receipt input")
	}
	data, err := json.Marshal(cacheReceipt{Version: 1, Kind: r.opts.Kind, Source: origin, Digest: digestOf(raw), Body: raw})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.opts.Dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(r.opts.Dir, ".catalog-receipt-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(r.opts.Dir, r.receiptName())); err != nil {
		return err
	}
	dir, err := os.Open(r.opts.Dir) //nolint:gosec // G304: operator-configured cache directory
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (r *Refresher) readReceipt() ([]byte, error) {
	f, err := os.Open(filepath.Join(r.opts.Dir, r.receiptName())) //nolint:gosec // G304: fixed file under operator-configured cache directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, r.receiptLimit()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > r.receiptLimit() {
		return nil, errors.New("catalogue: cache receipt exceeds bound")
	}
	var receipt cacheReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	if receipt.Version != 1 || receipt.Kind != r.opts.Kind || receipt.Source != r.opts.URL || len(receipt.Source) > maxSourceBytes || len(receipt.Body) == 0 || len(receipt.Body) > r.maxBytes() || receipt.Digest != digestOf(receipt.Body) {
		return nil, errors.New("catalogue: invalid cache source/content receipt")
	}
	// Only our exact versioned encoding is accepted: duplicate/case-alias keys,
	// omitted fields and trailing data cannot produce an ambiguous receipt.
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, data) {
		return nil, errors.New("catalogue: noncanonical cache receipt")
	}
	return receipt.Body, nil
}

// loadPersisted accepts only a complete receipt for the original configured
// source and kind. A rejected or legacy cache leaves the existing owner intact.
func (r *Refresher) loadPersisted() {
	if r.opts.Dir == "" {
		return
	}
	body, err := r.readReceipt()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.log.Warn("catalog: ignoring the kept download receipt", "err", err)
		}
		return
	}
	if r.opts.Kind == KindAdGuard {
		imp, err := ParseAdGuard(body, r.opts.URL)
		if err == nil {
			r.opts.Holder.SetImport(imp)
		}
		return
	}
	c, err := Parse(body, r.opts.URL)
	if err == nil {
		r.opts.Holder.Set(c)
	}
}
