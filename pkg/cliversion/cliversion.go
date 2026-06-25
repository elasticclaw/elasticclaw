package cliversion

import (
	"os"
	"strings"
)

const OpenClawVersion = "2026.6.9"

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
