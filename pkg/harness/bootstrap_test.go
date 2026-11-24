package harness

import "testing"

type testBootstrap struct {
	name string
}

func (b testBootstrap) Name() string                          { return b.name }
func (b testBootstrap) Config(BootstrapInput) BootstrapConfig { return BootstrapConfig{} }

func TestBootstrapRegistry(t *testing.T) {
	name := t.Name()
	t.Cleanup(func() {
		bootstrapRegistry.mu.Lock()
		defer bootstrapRegistry.mu.Unlock()
		delete(bootstrapRegistry.bootstraps, name)
	})
	if got, ok := LookupBootstrap(name); ok || got != nil {
		t.Fatalf("unregistered bootstrap = %v, %v", got, ok)
	}
	want := testBootstrap{name: name}
	RegisterBootstrap(want)
	if got, ok := LookupBootstrap(name); !ok || got != want {
		t.Fatalf("registered bootstrap = %v, %v; want %v, true", got, ok, want)
	}
	t.Run("duplicate", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate registration did not panic")
			}
		}()
		RegisterBootstrap(want)
	})
	if got, ok := LookupBootstrap(name); !ok || got != want {
		t.Fatalf("bootstrap changed after duplicate registration: %v, %v", got, ok)
	}
}
