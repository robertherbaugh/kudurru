//go:build !unix

package dhcp

import (
	"context"
	"fmt"
	"os"
)

func lockDNSClaims(context.Context, string) (*os.File, error) {
	return nil, fmt.Errorf("dhcp: durable DNS claim ordering requires Unix")
}
