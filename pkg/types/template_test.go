package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func testBoolPtr(b bool) *bool { return &b }

func TestLLMKeyConfigEnvVarName(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{provider: "anthropic", want: "ANTHROPIC_API_KEY"},
		{provider: "grok", want: "XAI_API_KEY"},
		{provider: "camel-stream", want: "CAMEL_STREAM_API_KEY"},
		{provider: "provider.name", want: "PROVIDER_NAME_API_KEY"},
		{provider: "123provider", want: "_123PROVIDER_API_KEY"},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			key := &LLMKeyConfig{Provider: tc.provider}
			if got := key.EnvVarName(); got != tc.want {
				t.Fatalf("EnvVarName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepositoryAccessListUnmarshalJSONHonorsClone(t *testing.T) {
	data := []byte(`[
		{"repo":"owner/clone-me","permissions":"write"},
		{"repo":"owner/*","permissions":"read","clone":false}
	]`)
	var list RepositoryAccessList
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := RepositoryAccessList{
		{Repo: "owner/clone-me", Permissions: "write"},
		{Repo: "owner/*", Permissions: "read", Clone: testBoolPtr(false)},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %#v, want %#v", list, want)
	}
}

func TestRepositoryAccessListUnmarshalJSONHonorsCloneTrue(t *testing.T) {
	data := []byte(`[{"repo":"owner/clone-me","permissions":"read","clone":true}]`)
	var list RepositoryAccessList
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := RepositoryAccessList{
		{Repo: "owner/clone-me", Permissions: "read", Clone: testBoolPtr(true)},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %#v, want %#v", list, want)
	}
}

func TestRepositoryAccessListUnmarshalYAMLHonorsClone(t *testing.T) {
	data := []byte(`repositories:
  - repo: owner/clone-me
    permissions: write
  - repo: owner/*
    permissions: read
    clone: false
`)
	type cfg struct {
		Repositories RepositoryAccessList `yaml:"repositories"`
	}
	var c cfg
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := RepositoryAccessList{
		{Repo: "owner/clone-me", Permissions: "write"},
		{Repo: "owner/*", Permissions: "read", Clone: testBoolPtr(false)},
	}
	if !reflect.DeepEqual(c.Repositories, want) {
		t.Fatalf("got %#v, want %#v", c.Repositories, want)
	}
}
