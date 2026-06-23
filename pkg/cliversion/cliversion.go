package cliversion

import (
	"os"
	"strings"
)

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
