package hub

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/install"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// supplyChainViolation reports a supply-chain check that could not be
// satisfied: it logs a warning and returns nil by default, or returns an error
// when security.strict_supply_chain is on.
func (s *Server) supplyChainViolation(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	s.mu.RLock()
	strict := s.hubCfg != nil && s.hubCfg.Security.StrictSupplyChain
	s.mu.RUnlock()
	if strict {
		return fmt.Errorf("[supply-chain] %w", err)
	}
	log.Printf("[supply-chain] WARNING %v", err)
	return nil
}

// checkContainerImage requires a container image reference pinned by digest.
func (s *Server) checkContainerImage(kind, image string) error {
	if _, digest, pinned := strings.Cut(image, "@sha256:"); !pinned || !validSHA256(digest) {
		return s.supplyChainViolation("%s image %q must be pinned with @sha256:<digest>", kind, image)
	}
	return nil
}

// daytonaSnapshot resolves the template snapshot, falling back to the hub
// default, and flags an empty result that would use Daytona's own default.
func (s *Server) daytonaSnapshot(req types.CreateClawRequest, cfg types.ProviderConfig) (string, error) {
	snapshot := req.Snapshot
	if snapshot == "" {
		snapshot = cfg.DefaultSnapshot
	}
	if snapshot == "" {
		return "", s.supplyChainViolation("Daytona snapshot is empty; provisioning uses Daytona's unpinned default snapshot")
	}
	return snapshot, nil
}

func bridgeLocalHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || strings.EqualFold(host, "host.docker.internal") || (ip != nil && ip.IsLoopback())
}

func (s *Server) checkBridgeTransport(ref string) error {
	u, err := url.Parse(ref)
	if err != nil {
		return s.supplyChainViolation("invalid bridge source %q: %v", ref, err)
	}
	if u.Scheme == "http" && !bridgeLocalHost(u.Hostname()) {
		return s.supplyChainViolation("bridge source requires HTTPS: %s", ref)
	}
	if strings.Contains(ref, "://") && u.Scheme != "https" && u.Scheme != "http" {
		return s.supplyChainViolation("unsupported bridge source: %s", ref)
	}
	return nil
}

func releaseBridgeSHA256(ctx context.Context, client *http.Client, ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	asset := path.Base(u.Path)
	u.Path = path.Dir(u.Path)
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	checksums, err := install.FetchReleaseChecksums(ctx, client, u.String())
	if err != nil {
		return "", err
	}
	digest, ok := checksums[asset]
	if !ok {
		return "", fmt.Errorf("release checksums missing %q", asset)
	}
	return digest, nil
}

// bridgeSHA256 returns the expected claw-bridge SHA256 for ref, or "" when it
// cannot be determined and strict mode is off. Release checksums are fetched
// once per URL; failures are not cached so a transient fetch error can recover.
// A pinned OCI digest is verified by oras itself, so it yields "".
func (s *Server) bridgeSHA256(ctx context.Context, ref string) (string, error) {
	if err := s.checkBridgeTransport(ref); err != nil {
		return "", err
	}
	digest, err := s.resolveBridgeSHA256(ctx, ref)
	if err != nil {
		return "", s.supplyChainViolation("cannot verify claw-bridge %q: %v", ref, err)
	}
	return digest, nil
}

func (s *Server) resolveBridgeSHA256(ctx context.Context, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, githubReleasesBase+"/"):
		s.bridgeChecksumMu.Lock()
		defer s.bridgeChecksumMu.Unlock()
		if digest, ok := s.bridgeChecksums[ref]; ok {
			return digest, nil
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		digest, err := releaseBridgeSHA256(fetchCtx, http.DefaultClient, ref)
		if err != nil {
			return "", err
		}
		if s.bridgeChecksums == nil {
			s.bridgeChecksums = make(map[string]string)
		}
		s.bridgeChecksums[ref] = digest
		return digest, nil
	case strings.HasPrefix(ref, "https://"), strings.HasPrefix(ref, "http://"):
		s.mu.RLock()
		var digest string
		if s.hubCfg != nil {
			digest = s.hubCfg.BridgeSHA256
		}
		s.mu.RUnlock()
		if !validSHA256(digest) {
			return "", fmt.Errorf("bridge_sha256 is missing or invalid")
		}
		return strings.ToLower(digest), nil
	default:
		if _, digest, pinned := strings.Cut(ref, "@sha256:"); !pinned || !validSHA256(digest) {
			return "", fmt.Errorf("OCI bridge source must be pinned with @sha256:<digest>")
		}
		return "", nil
	}
}

func verifiedDockerBridgeBytes(data []byte, digest string) ([]byte, error) {
	if digest == "" {
		return data, nil
	}
	if err := install.VerifySHA256(bytes.NewReader(data), digest); err != nil {
		return nil, fmt.Errorf("docker claw-bridge verification: %w", err)
	}
	return data, nil
}

var npmExactVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var uvExactVersion = regexp.MustCompile(`^([0-9]+!)?[0-9]+(\.[0-9]+)*((a|b|rc)[0-9]+)?(\.post[0-9]+)?(\.dev[0-9]+)?(\+[a-zA-Z0-9]+([._-][a-zA-Z0-9]+)*)?$`)

func mcpPackagePinned(source types.MCPSource, pkg string) bool {
	switch source {
	case types.MCPSourceNpx:
		i := strings.LastIndex(pkg, "@")
		return i > 0 && npmExactVersion.MatchString(pkg[i+1:])
	case types.MCPSourceUvx:
		name, version, ok := strings.Cut(pkg, "==")
		return ok && name != "" && uvExactVersion.MatchString(version)
	default:
		return true
	}
}

func (s *Server) checkMCPPackage(source types.MCPSource, pkg string) error {
	if !mcpPackagePinned(source, pkg) {
		return s.supplyChainViolation("MCP %s package %q must have an exact version", source, pkg)
	}
	return nil
}
