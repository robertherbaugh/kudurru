package rpz

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

// acquisition is retained by one scheduler entry. Replacing its installation
// creates a new entry and cancels the old worker without reusing its budget.
type acquisition struct {
	keys     dnsserver.AcquisitionKeys
	receiver Receiver
}

func (a acquisition) current() bool { return a.keys == nil || a.keys.Current() }

func (a acquisition) identity() string {
	if a.keys == nil {
		return ""
	}
	return a.keys.Identity()
}

func (a acquisition) withCurrent(ctx context.Context, fn func() error) error {
	return a.accept(ctx, fn, nil)
}

func (a acquisition) accept(ctx context.Context, cache func() error, publish func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.keys == nil {
		if err := cache(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if publish != nil {
			return publish()
		}
		return nil
	}
	if !a.current() {
		return errors.New("rpz: original credential installation changed")
	}
	return a.keys.WithCurrent(ctx, cache, publish)
}

func (m *Manager) captureAcquisition() (acquisition, error) {
	if m.opts.CaptureKeys == nil {
		return acquisition{receiver: m.opts.Receiver}, nil
	}
	keys := m.opts.CaptureKeys()
	if keys == nil || keys.Keys() == nil || keys.Identity() == "" || !keys.Current() {
		return acquisition{}, errors.New("rpz: immutable credential source is unavailable")
	}
	return acquisition{keys: keys}, nil
}

// prepareReceiver is called only after entry identity comparison. Stable
// reconciliation and final credential checks never construct extra receivers.
func (m *Manager) prepareReceiver(a *acquisition) error {
	if a.keys == nil {
		return nil
	}
	a.receiver = m.opts.ReceiverForKeys(a.keys.Keys())
	if a.receiver == nil {
		return errors.New("rpz: captured receiver is unavailable")
	}
	return nil
}

// jobCredentialsMatch never captures a successor provider for a captured job.
// The legacy path retains its existing mutable-provider comparison separately.
func (m *Manager) jobCredentialsMatch(job transferJob) bool {
	if m.opts.CaptureKeys == nil {
		return m.credentialsMatch(job.dz, job.credential)
	}
	if job.acquisition.keys == nil || !job.acquisition.current() {
		return false
	}
	fingerprint, err := acquisitionCredential(job.dz, job.acquisition.keys.Keys())
	return err == nil && fingerprint == job.credential
}

func acquisitionCredential(dz *v1alpha1.DNSZone, keys dnsserver.TSIGKeyring) (string, error) {
	if dz == nil || dz.Spec.Secondary == nil || len(dz.Spec.Secondary.Primaries) == 0 {
		return "", errors.New("rpz: invalid transfer configuration")
	}
	if dz.Spec.Secondary.TSIGKeyRef == "" {
		return "unsigned", nil
	}
	fingerprint, ok := dnsserver.AcquisitionFingerprint(keys, dz.Spec.Secondary.TSIGKeyRef)
	if !ok {
		return "", errors.New("rpz: TSIG key is unavailable")
	}
	return fingerprint, nil
}

func (m *Manager) captureEntry(dz *v1alpha1.DNSZone) (acquisition, string, error) {
	a, err := m.captureAcquisition()
	if err != nil {
		return a, "", err
	}
	keys := m.opts.Keyring
	if a.keys != nil {
		keys = a.keys.Keys()
	}
	fingerprint, err := acquisitionCredential(dz, keys)
	return a, fingerprint, err
}

// receiverFor keeps package-internal legacy direct refresh fixtures working;
// captured production entries always contain their own receiver.
func (m *Manager) receiverFor(job transferJob) Receiver {
	if job.acquisition.receiver != nil {
		return job.acquisition.receiver
	}
	if m.opts.CaptureKeys != nil {
		return nil
	}
	return m.opts.Receiver
}
