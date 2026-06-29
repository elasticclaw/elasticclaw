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
		{"factory wins over template and default", "replicated", "daytona", "exedev", "replicated", false},
		{"template wins when factory empty", "", "daytona", "exedev", "daytona", false},
		{"default wins when factory and template empty", "", "", "exedev", "exedev", false},
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

func TestDefaultProviderFallsBackToSingleConfiguredProvider(t *testing.T) {
	cases := []struct {
		name      string
		provider  map[string]types.ProviderConfig
		want      string
		wantEmpty bool
	}{
		{
			name: "single docker without credentials",
			provider: map[string]types.ProviderConfig{
				"docker": {},
			},
			want: "docker",
		},
		{
			name: "single empty credentialed provider",
			provider: map[string]types.ProviderConfig{
				"daytona": {},
			},
			want: "daytona",
		},
		{
			name: "credentialed provider wins over single-provider fallback",
			provider: map[string]types.ProviderConfig{
				"docker":  {},
				"daytona": {APIKey: "daytona-key"},
			},
			want: "daytona",
		},
		{
			name: "multiple credentialless providers do not guess",
			provider: map[string]types.ProviderConfig{
				"docker": {},
				"exedev": {},
			},
			wantEmpty: true,
		},
		{
			name: "noop stub is ignored",
			provider: map[string]types.ProviderConfig{
				"noop": {Type: "noop"},
			},
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{hubCfg: &types.HubConfig{Providers: tc.provider}}
			got := s.defaultProvider()
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("defaultProvider() = %q, want empty", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("defaultProvider() = %q, want %q", got, tc.want)
			}
		})
	}
}
