package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

func TestDaytonaInstallBrowserRuntimeCommandUsesBundledPlaywright(t *testing.T) {
	cmd := daytonaInstallBrowserRuntimeCommand()
	for _, want := range []string{
		"playwright-core/cli.js",
		"PLAYWRIGHT_BROWSERS_PATH",
		"install --with-deps chromium",
		"export HOME=/home/daytona",
		`$HOME/.cache/ms-playwright`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("browser runtime command missing %q:\n%s", want, cmd)
		}
	}
}

func TestDaytonaInstallBrowserUseRuntimeCommandInstallsPinnedCLIAndVideo(t *testing.T) {
	cmd := daytonaInstallBrowserUseRuntimeCommand()
	for _, want := range []string{
		"uv tool install --python 3.12",
		"browser-use[video]==" + cliversion.BrowserUseVersion,
		"browser-use install",
		"browser-use doctor",
		"browser-use record --help",
		"ffmpeg -version",
		"export HOME=/home/daytona",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("Browser Use runtime command missing %q:\n%s", want, cmd)
		}
	}
}
