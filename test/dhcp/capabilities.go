//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Validate actual process credentials, not just setpriv's requested flags.
// This is the shared least-privilege contract of the systemd drop-in and
// opt-in host-network container image; it does not emulate either manager.
func checkCapabilities() error {
	if os.Geteuid() != 65532 {
		return fmt.Errorf("server must run as uid65532")
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found {
			fields[key] = strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"CapEff", "CapPrm", "CapBnd"} {
		value, err := strconv.ParseUint(fields[key], 16, 64)
		if err != nil || value != 0x2400 {
			return fmt.Errorf("%s must contain exactly NET_BIND_SERVICE and NET_RAW; got %s", key, fields[key])
		}
	}
	if profile := os.Getenv("KUDURRU_DHCP_CAPABILITY_PROFILE"); profile != "" {
		ambient, parseErr := strconv.ParseUint(fields["CapAmb"], 16, 64)
		expected := uint64(0)
		if profile == "ambient" {
			expected = 0x2400
		}
		if parseErr != nil || ambient != expected {
			return fmt.Errorf("unexpected ambient capabilities for %s profile: %s", profile, fields["CapAmb"])
		}
	}
	if fields["NoNewPrivs"] != "1" {
		return fmt.Errorf("no-new-privileges must be enabled")
	}
	// Opening a raw socket proves that the capability is effective. No packet
	// is sent, and main already required the disconnected namespace marker.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("raw capability ineffective: %w", err)
	}
	return unix.Close(fd)
}
