package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPreviewProviderHelpers(t *testing.T) {
	if got := previewPorts(0); got != nil {
		t.Fatalf("previewPorts(0) = %#v, want nil", got)
	}
	ports := previewPorts(3000)
	if len(ports) != 1 || ports[0] != 3000 {
		t.Fatalf("previewPorts(3000) = %#v", ports)
	}

	instance := &types.Instance{
		ProviderMeta: map[string]string{"preview_url_3000": "https://preview.example"},
	}
	if got := instancePreviewURL(instance, 3000); got != "https://preview.example" {
		t.Fatalf("instancePreviewURL() = %q", got)
	}
	if got := instancePreviewURL(instance, 8080); got != "" {
		t.Fatalf("missing instancePreviewURL() = %q, want empty", got)
	}
}
