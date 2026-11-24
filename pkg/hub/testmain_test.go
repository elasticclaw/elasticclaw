package hub

import (
	"os"
	"testing"

	"github.com/elasticclaw/elasticclaw/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunWithIsolatedHome(m))
}
