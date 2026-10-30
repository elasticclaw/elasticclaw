// Package install manages the opt-in, native observability stack.
package install

const (
	BinaryDir         = "/opt/elasticclaw-observability"
	ConfigDir         = "/etc/elasticclaw-observability"
	DataDir           = "/var/lib/elasticclaw-observability"
	UnitDir           = "/etc/systemd/system"
	MarkerFile        = ConfigDir + "/installed"
	PasswordFile      = ConfigDir + "/grafana-admin-password"
	ServiceUser       = "elasticclaw-obs"
	LogsPluginVersion = "0.33.1"
)

type Component struct {
	Name, Version, URL, Binary, AMD64, ARM64, Memory, Health string
}

// Components pins official release archives and their published SHA256 checksums.
var Components = []Component{
	{"collector", "0.164.0", "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.164.0/otelcol-contrib_0.164.0_linux_%s.tar.gz", "otelcol-contrib", "f0c1ce9f661399beb4a58b711924716e08ea70c3802db0715fcfde3cbbada2d4", "8bd4a7f443405522a2175cc1a8675f9e99cc047c7181b73461efd08efbba38d3", "512M", "http://127.0.0.1:13133/"},
	{"victoriametrics", "v1.155.0", "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.155.0/victoria-metrics-linux-%s-v1.155.0.tar.gz", "victoria-metrics-prod", "7180e65913d55ab99c0be8f4a3ad17a0fafe5598a21ff6327fecba3190d7b176", "c6af3f35c597e2cb45c9bbb7a7fc1638c6f1ec10a8198c2861aa14531bf439d7", "512M", "http://127.0.0.1:8428/health"},
	{"victorialogs", "v1.53.0", "https://github.com/VictoriaMetrics/VictoriaLogs/releases/download/v1.53.0/victoria-logs-linux-%s-v1.53.0.tar.gz", "victoria-logs-prod", "55feba89713cafa952673f91b8b38e7701bb0290989263da671b47a0de16edd3", "301b7ef12ff9fac7f70b155191affe07ac6212755514174461d775548699ef96", "512M", "http://127.0.0.1:9428/health"},
	{"tempo", "3.1.0", "https://github.com/grafana/tempo/releases/download/v3.1.0/tempo_3.1.0_linux_%s.tar.gz", "tempo", "9604ab46348270a42954801bba91cd65335e16568ef886bdf71b988fdcb7ac4e", "e4092dcf2b93e141be77e300b539f6c5cceefa9803417d7d635c6797d9bc19ad", "768M", "http://127.0.0.1:3200/ready"},
	{"grafana", "12.4.3", "https://dl.grafana.com/oss/release/grafana-12.4.3.linux-%s.tar.gz", "bin/grafana", "64707f35d54a5441ecbc63c0b3b633b7e73fe404fac118e88574f56d70b08a93", "825e31eb0aafcf026d7f04dd540042eedef42cb81a3d2832c296b27dacf7e95d", "384M", "http://127.0.0.1:3001/grafana/api/health"},
}

func unitName(name string) string { return "elasticclaw-obs-" + name + ".service" }
