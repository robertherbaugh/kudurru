//go:build !unix

package dhcpsetup

import (
	"fmt"
	"os"
)

func privateFile(string, bool) (*os.File, error) {
	return nil, fmt.Errorf("dhcpsetup: private setup storage requires Unix")
}
func acquireLock(string) (*os.File, error) {
	return nil, fmt.Errorf("dhcpsetup: exclusive setup ownership requires Unix")
}
