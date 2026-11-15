package main

import (
	"errors"
	"log/slog"
	"net"
	"strconv"

	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func (f *edgeFlags) managedObservationServer(s *dnsdist.Supervisor, logger *slog.Logger, creds *internalapi.Credentials) (*internalapi.Server, error) {
	if f.managedObservationAddr == "" {
		return nil, nil
	}
	_, port, err := net.SplitHostPort(f.managedObservationAddr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || !f.managedCacheBypass || s == nil {
		return nil, errors.New("managed edge observation needs a listener host:port and retained cache bypass")
	}
	if creds == nil {
		return nil, internalapi.ErrNoCredentials
	}
	return internalapi.NewServer(internalapi.ServerOptions{
		Address: f.managedObservationAddr, Credentials: creds, Logger: logger, EdgeBypass: s.ManagedBypass,
	})
}
