//go:build !linux && !darwin

package install

import "fmt"

func freeDisk(string) (uint64, error) {
	return 0, fmt.Errorf("observability installation requires Linux")
}
