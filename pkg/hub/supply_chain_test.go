package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestBridgeSupplyChainPolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s := &Server{hubCfg: &types.HubConfig{}}
	if _, err := s.bridgeSHA256(context.Background(), "http://example.com/bridge"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "[supply-chain] WARNING bridge source requires HTTPS") {
		t.Fatalf("missing warning: %s", &output)
	}
	s.hubCfg.Security.StrictSupplyChain = true
	if _, err := s.bridgeSHA256(context.Background(), "http://example.com/bridge"); err == nil {
		t.Fatal("strict mode accepted HTTP")
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "host.docker.internal"} {
		if err := s.checkBridgeTransport("http://" + host + "/bridge"); err != nil {
			t.Fatal(err)
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("bridge")))
	s.hubCfg.BridgeSHA256 = digest
	if got, err := s.bridgeSHA256(context.Background(), "https://example.com/bridge"); err != nil || got != digest {
		t.Fatalf("custom checksum: %s, %v", got, err)
	}
	for _, ref := range []string{"ghcr.io/test/bridge:latest", "ghcr.io/test/bridge@sha256:invalid"} {
		if _, err := s.bridgeSHA256(context.Background(), ref); err == nil {
			t.Fatalf("accepted %s", ref)
		}
	}
	if got, err := s.bridgeSHA256(context.Background(), "ghcr.io/test/bridge@sha256:"+digest); err != nil || got != "" {
		t.Fatalf("pinned OCI: %s, %v", got, err)
	}
}

func TestReleaseBridgeSHA256(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("bridge")))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checksums.txt" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "%s  claw-bridge-linux-amd64\n", digest)
	}))
	defer server.Close()
	got, err := releaseBridgeSHA256(context.Background(), server.Client(), server.URL+"/v1/claw-bridge-linux-amd64")
	if err != nil || got != digest {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := releaseBridgeSHA256(context.Background(), server.Client(), server.URL+"/v1/missing"); err == nil {
		t.Fatal("accepted missing asset")
	}
}

type checksumTransport func(*http.Request) (*http.Response, error)

func (f checksumTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBridgeChecksumCache(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("bridge")))
	var calls atomic.Int32
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: checksumTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != githubReleasesBase+"/v1/checksums.txt" {
			t.Errorf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(digest + "  claw-bridge-linux-amd64\n")), Header: make(http.Header)}, nil
	})}
	defer func() { http.DefaultClient = previous }()
	s := &Server{hubCfg: &types.HubConfig{Security: types.SecurityConfig{StrictSupplyChain: true}}}
	for i := 0; i < 2; i++ {
		got, err := s.bridgeSHA256(context.Background(), githubReleasesBase+"/v1/claw-bridge-linux-amd64")
		if err != nil || got != digest {
			t.Fatalf("got %s, %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times", calls.Load())
	}
}

func TestBridgeDownloadScriptsVerifySHA256(t *testing.T) {
	p := baseParams()
	p.BridgeSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("bridge")))
	for name, script := range map[string]string{"replicated": GenerateReplicatedBootstrapScript(p), "daytona": daytonaBridgeDownloadScript(p.BridgeURL, p.BridgeSHA256)} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid shell syntax: %v: %s", err, output)
			}
			for _, fragment := range []string{"BRIDGE_SHA256=" + shellQuote(p.BridgeSHA256), `echo "$BRIDGE_SHA256  $BRIDGE_PATH" | sha256sum -c -`, cliversion.OrasLinuxAMD64SHA256, "ERROR: claw-bridge SHA256 mismatch", "ERROR: oras SHA256 mismatch", "--proto '=https'", "oras_" + cliversion.OrasVersion, "[supply-chain] WARNING"} {
				if !strings.Contains(script, fragment) {
					t.Errorf("missing %q", fragment)
				}
			}
		})
	}
}

func TestDockerBridgeVerification(t *testing.T) {
	data := []byte("bridge")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if _, err := verifiedDockerBridgeBytes(data, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedDockerBridgeBytes([]byte("modified"), digest); err == nil {
		t.Fatal("accepted modified bridge")
	}
}

func TestBridgeChecksumFailureHonorsStrictMode(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{}}
	ref := "https://example.com/bridge"
	if _, err := s.bridgeSHA256(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	s.hubCfg.Security.StrictSupplyChain = true
	if _, err := s.bridgeSHA256(context.Background(), ref); err == nil {
		t.Fatal("strict mode accepted checksum failure")
	}
}

func TestDockerBridgeDownloadVerification(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: checksumTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("bridge")), Header: make(http.Header)}, nil
	})}
	defer func() { http.DefaultClient = previous }()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("bridge")))
	data, err := downloadDockerBridgeBinary(context.Background(), "https://example.com/bridge", digest)
	if err != nil || string(data) != "bridge" {
		t.Fatalf("download got %q, %v", data, err)
	}
	if _, err := downloadDockerBridgeBinary(context.Background(), "https://example.com/bridge", strings.Repeat("0", 64)); err == nil {
		t.Fatal("download accepted checksum mismatch")
	}
}

func TestContainerImageSupplyChainPolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, kind := range []string{"docker provider", "MCP docker"} {
		for _, image := range []string{"", "image:latest", "image@sha256:invalid", cliversion.OpenClawImage} {
			for _, strict := range []bool{false, true} {
				output.Reset()
				s := &Server{hubCfg: &types.HubConfig{Security: types.SecurityConfig{StrictSupplyChain: strict}}}
				err := s.checkContainerImage(kind, image)
				unpinned := image != cliversion.OpenClawImage
				if (err != nil) != (strict && unpinned) {
					t.Fatalf("%s image=%q strict=%v: %v", kind, image, strict, err)
				}
				if warned := strings.Contains(output.String(), "[supply-chain] WARNING "+kind); warned != (!strict && unpinned) {
					t.Fatalf("image=%q strict=%v: unexpected warning %q", image, strict, output.String())
				}
			}
		}
	}
}

func TestDaytonaSnapshotSupplyChainPolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, tc := range []struct{ requested, configured, want string }{
		{"", "", ""},
		{"", "hub-snapshot", "hub-snapshot"},
		{"template-snapshot", "hub-snapshot", "template-snapshot"},
	} {
		for _, strict := range []bool{false, true} {
			output.Reset()
			s := &Server{hubCfg: &types.HubConfig{Security: types.SecurityConfig{StrictSupplyChain: strict}}}
			got, err := s.daytonaSnapshot(types.CreateClawRequest{Snapshot: tc.requested}, types.ProviderConfig{DefaultSnapshot: tc.configured})
			if got != tc.want || (err != nil) != (strict && tc.want == "") {
				t.Fatalf("snapshot=%q default=%q strict=%v: got %q, %v", tc.requested, tc.configured, strict, got, err)
			}
			if warned := strings.Contains(output.String(), "[supply-chain] WARNING Daytona snapshot is empty"); warned != (!strict && tc.want == "") {
				t.Fatalf("unexpected warning %q", output.String())
			}
		}
	}
}

func TestStrictImagePolicyStopsProvisioning(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{Security: types.SecurityConfig{StrictSupplyChain: true}}}
	for _, provider := range []string{"docker", "daytona"} {
		err := s.provisionWithProvider(context.Background(), provider, "claw", types.CreateClawRequest{}, types.ProviderConfig{Image: "image:latest"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "[supply-chain]") {
			t.Fatalf("%s: expected supply-chain rejection before provider initialization, got %v", provider, err)
		}
	}
}

func TestStrictMCPImagePolicyRejectsCreate(t *testing.T) {
	s, db, _ := newClawRetryTestServer(t, "provisioning")
	s.hubCfg.Security.StrictSupplyChain = true
	s.hubCfg.Providers = map[string]types.ProviderConfig{"docker": {}}
	s.hubCfg.MCPServers = []*types.MCPServerHubConfig{{Name: "mcp", Source: types.MCPSourceDocker, Image: "image:latest", Enabled: true}}
	request := httptest.NewRequest(http.MethodPost, "/api/claws", strings.NewReader(`{"name":"test","provider":"docker","mcps":[{"name":"mcp"}]}`))
	response := httptest.NewRecorder()
	s.handleCreateClaw(response, request, "tenant")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "[supply-chain] MCP docker") {
		t.Fatalf("got %d: %s", response.Code, response.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE name='test'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected request created a claw: count=%d err=%v", count, err)
	}
}

func TestMCPPackagePinned(t *testing.T) {
	for _, tc := range []struct {
		source types.MCPSource
		pkg    string
		want   bool
	}{
		{types.MCPSourceNpx, "server@1.2.3", true},
		{types.MCPSourceNpx, "@scope/server@1.2.3-rc.1+build", true},
		{types.MCPSourceNpx, "@scope/server", false},
		{types.MCPSourceNpx, "server", false},
		{types.MCPSourceNpx, "server@latest", false},
		{types.MCPSourceNpx, "server@^1.2.3", false},
		{types.MCPSourceNpx, "server@~1.2.3", false},
		{types.MCPSourceNpx, "server@1.2", false},
		{types.MCPSourceNpx, "server@1.2.*", false},
		{types.MCPSourceUvx, "server==1.2.3", true},
		{types.MCPSourceUvx, "server[cli]==1.2.3rc1", true},
		{types.MCPSourceUvx, "server==1.2.3.post1", true},
		{types.MCPSourceUvx, "server", false},
		{types.MCPSourceUvx, "server>=1.2.3", false},
		{types.MCPSourceUvx, "server==1.*", false},
		{types.MCPSourceUvx, "server==1.2,<=2", false},
		{types.MCPSourceUvx, "server==", false},
	} {
		t.Run(string(tc.source)+"/"+tc.pkg, func(t *testing.T) {
			if got := mcpPackagePinned(tc.source, tc.pkg); got != tc.want {
				t.Fatalf("pinned = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMCPPackageSupplyChainPolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s := &Server{hubCfg: &types.HubConfig{}}
	for _, source := range []types.MCPSource{types.MCPSourceNpx, types.MCPSourceUvx} {
		s.hubCfg.Security.StrictSupplyChain = false
		output.Reset()
		if err := s.checkMCPPackage(source, "server"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "[supply-chain] WARNING MCP") {
			t.Fatalf("missing warning: %s", &output)
		}
		s.hubCfg.Security.StrictSupplyChain = true
		if err := s.checkMCPPackage(source, "server"); err == nil {
			t.Fatal("strict mode must skip unpinned MCP")
		}
	}
	for _, tc := range []struct {
		source types.MCPSource
		pkg    string
	}{
		{types.MCPSourceNpx, "server@1.2.3"},
		{types.MCPSourceUvx, "server==1.2.3"},
		{types.MCPSourceSmithery, "server"},
	} {
		if err := s.checkMCPPackage(tc.source, tc.pkg); err != nil {
			t.Fatal(err)
		}
	}
}
