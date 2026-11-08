//go:build !unix

package dnsdist

import "errors"

func acquireConfigState(string, bool) (*configState, error) {
	return nil, errors.New("dnsdist: exclusive configuration directory ownership requires Unix")
}
