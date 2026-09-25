package cliversion

import (
	"os"
	"strings"
)

const (
	OpenClawVersion      = "2026.9.4"
	OpenClawImageVersion = "2026.9.4"
	OpenClawImage        = "ghcr.io/openclaw/openclaw:" + OpenClawImageVersion
	CodexPluginVersion   = "2026.9.4"
	CodexCLIVersion      = "0.153.4"
	GrokCLIVersion       = "0.2.103"
)

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
