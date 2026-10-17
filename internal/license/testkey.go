//go:build licensetest

package license

// This file is compiled only with -tags licensetest, which the Playwright
// suite (make dashboard-e2e) and nothing else uses. It makes a binary trust a
// throwaway key whose private half was discarded after it signed
// testdata/e2e-license.key (all features, 10 nodes, expires in 2126), so the
// dashboard suite can run a real server with a license without the licensor's
// signing key. A release build never carries it: TestDefaultBuildTrustsNoTestKey.

func init() {
	pub, err := ParsePublicKey("RbzaQR8cNwJyeKs5uS9TJ7+iUQ0Flu1yge2RgHQlWjs=")
	if err != nil {
		panic(err)
	}
	testKeys = Keyring{"e2e-test": pub}
}
