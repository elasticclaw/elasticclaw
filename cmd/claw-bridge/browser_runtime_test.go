package main

import (
	"strings"
	"testing"
)

func TestOpenClawBrowserRuntimeInstallScriptUsesBundledPlaywright(t *testing.T) {
	script := openClawBrowserRuntimeInstallScript()
	for _, want := range []string{
		"playwright-core/cli.js",
		"PLAYWRIGHT_BROWSERS_PATH",
		"install --with-deps chromium",
		`$HOME/.cache/ms-playwright`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("browser runtime script missing %q:\n%s", want, script)
		}
	}
}

func TestBrowserUseRuntimeInstallScriptIncludesVideoAndMediaTools(t *testing.T) {
	version := "0.13.1-test"
	script := browserUseRuntimeInstallScript(version)
	for _, want := range []string{
		"browser-use[video]==" + version,
		"uv tool install --python 3.12",
		"browser-use install",
		"browser-use doctor",
		"browser-use record --help",
		"ffmpeg -version",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Browser Use runtime script missing %q:\n%s", want, script)
		}
	}
}
