package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func testBoolPtr(b bool) *bool { return &b }

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
