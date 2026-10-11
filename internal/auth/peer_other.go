//go:build !linux

package auth

import "net"

// ReadPeerCred is unavailable off Linux: the admin socket then admits no
// peer as the local root.
func ReadPeerCred(net.Conn) (PeerCred, error) { return PeerCred{}, ErrNoPeerCred }
