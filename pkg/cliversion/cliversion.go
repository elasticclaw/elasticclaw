package cliversion

import (
	"os"
	"strings"
)

const (
	OpenClawVersion      = "2026.7.1-2"
	OpenClawImageVersion = "2026.7.1"
	OpenClawImage        = "ghcr.io/openclaw/openclaw:" + OpenClawImageVersion
)

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
