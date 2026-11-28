package authsvc

import "os"

// providerstest_write.go: shared test helper (callable from any _test file in
// this package without clashing with production identifiers).
func providerTestWriteFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
