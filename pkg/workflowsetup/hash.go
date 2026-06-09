package workflowsetup

import (
	"crypto/sha256"
	"fmt"
)

// ConfigHash returns a deterministic hash for a rendered workflow config.
func ConfigHash(config string) string {
	sum := sha256.Sum256([]byte(config))
	return fmt.Sprintf("sha256:%x", sum)
}
