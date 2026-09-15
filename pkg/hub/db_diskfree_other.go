//go:build !unix

package hub

import "errors"

// diskFreeBytesOS has no answer here, which the gate treats as "attempt the
// step"; the truncate after a failure still applies.
func diskFreeBytesOS(string) (int64, error) {
	return 0, errors.New("free disk space is not queried on this platform")
}
