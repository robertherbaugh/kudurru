package host

import (
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
)

// ObservationScheduler optionally describes a read-only Observer's negotiated
// polling capability after Check. Zero disables automatic and manual inventory
// sync; a positive duration is its maximum polling interval. The answer must be immutable
// for the provider incarnation, perform no I/O and grant no source authority.
// Resync may shorten that interval, never an observation's original expiry.
// The bound also applies when a stored definition carries the generic CRD default.
// Providers without this interface retain their existing scheduling behavior.
type ObservationScheduler interface {
	ObservationSchedule() (time.Duration, error)
}

func syncSchedule(provider integration.Integration, definition *public.Integration) (bool, time.Duration, error) {
	interval := definition.Spec.Resync.Duration
	if optional, ok := provider.(ObservationScheduler); ok {
		if _, observer := provider.(Observer); !observer {
			return false, 0, ErrInvalid
		}
		if _, publisher := provider.(Publisher); publisher {
			return false, 0, ErrInvalid
		}
		cadence, err := optional.ObservationSchedule()
		if err != nil {
			return false, 0, err
		}
		if cadence == 0 {
			return false, 0, nil
		}
		if cadence < time.Second || cadence > 24*time.Hour {
			return false, 0, ErrInvalid
		}
		if interval == 0 || interval > cadence {
			interval = cadence
		}
	} else {
		if interval == 0 {
			interval = 10 * time.Minute
		}
		if u := definition.Spec.UniFi; u != nil {
			interval = u.Poll.Duration
			if interval == 0 {
				interval = time.Minute
			}
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	return true, interval, nil
}
