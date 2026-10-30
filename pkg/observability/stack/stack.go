// Package stack holds the shared Docker and native backend configuration.
package stack

import "embed"

//go:embed collector.yaml tempo.yaml grafana/provisioning/datasources/datasources.yaml
var Files embed.FS
