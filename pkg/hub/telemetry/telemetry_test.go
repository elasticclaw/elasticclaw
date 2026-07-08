package telemetry

import (
	"context"
	"testing"
)

func TestSetupWithoutEndpointIsNoop(t *testing.T) {
	t.Setenv(EndpointEnv, "")
	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if Enabled() {
		t.Fatal("tracing must stay disabled when the endpoint is unset")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}

	// With tracing disabled, provider spans must pass the context through
	// unchanged and the end func must be callable.
	ctx := context.Background()
	spanCtx, end := StartProviderSpan(ctx, "create", "daytona")
	if spanCtx != ctx {
		t.Fatal("disabled StartProviderSpan must return the same context")
	}
	end(nil)
}
