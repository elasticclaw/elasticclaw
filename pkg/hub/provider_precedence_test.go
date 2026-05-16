package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestResolveProviderPrecedence(t *testing.T) {
	cases := []struct {
		name         string
		factory      string // factory.Provider
		tmpl         string // tmplCfg.Provider
		hubDefault   string // s.defaultProvider() result
		wantProvider string
		wantErr      bool
	}{
		{"factory wins over template and default", "replicated", "daytona", "vercel", "replicated", false},
		{"template wins when factory empty", "", "daytona", "vercel", "daytona", false},
		{"default wins when factory and template empty", "", "", "vercel", "vercel", false},
		{"error when all empty", "", "", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := &types.FactoryConfig{Provider: tc.factory}
			var tmplCfg *types.TemplateConfig
			if tc.tmpl != "" {
				tmplCfg = &types.TemplateConfig{Provider: tc.tmpl}
			}

			provider, err := resolveProvider(factory, tmplCfg, tc.hubDefault)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got provider %q", provider)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider: want %q, got %q", tc.wantProvider, provider)
			}
		})
	}
}
