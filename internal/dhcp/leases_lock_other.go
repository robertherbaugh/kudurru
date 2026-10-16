//go:build !unix

package dhcp

import (
	"fmt"
	"os"
)

func lockLeaseFile(string) (*os.File, error) {
	return nil, fmt.Errorf("dhcp: exclusive lease ownership requires Unix")
}

func privateLeaseFile(string, bool) (*os.File, error) {
	return nil, fmt.Errorf("dhcp: private lease storage requires Unix")
}

func readOnlyLeaseFile(string) (*os.File, error) {
	return nil, fmt.Errorf("dhcp: private lease inspection requires Unix")
}
