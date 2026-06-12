package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSyncVolumeClosesPipeOnPutError(t *testing.T) {
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("put failed before reading body")
		}),
	}
	defer func() {
		http.DefaultClient = oldClient
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := syncVolume(ctx, types.VolumeSyncPayload{
		LeaseID:   "lease-1",
		Mode:      "rw",
		Mount:     t.TempDir(),
		HubURL:    "http://hub.example",
		ClawToken: "token",
	})
	if err == nil || !strings.Contains(err.Error(), "put failed before reading body") {
		t.Fatalf("syncVolume error = %v, want transport error", err)
	}
}
