package cliversion

import (
	"os"
	"strings"
)

const (
	OpenClawVersion      = "2026.7.1-2"
	OpenClawImageVersion = "2026.7.1"
	OpenClawImage        = "ghcr.io/openclaw/openclaw:" + OpenClawImageVersion
	CodexPluginVersion   = "2026.7.1-1"
	CodexCLIVersion      = "0.144.6"
	GrokCLIVersion       = "0.2.103"
	// Pin the isolated, named-session CLI version validated by the browser
	// evidence bootstrap and recording workflow.
	BrowserUseVersion = "0.13.1"
)

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
