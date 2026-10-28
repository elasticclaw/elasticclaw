package hub

import (
	"context"
	"fmt"
	"os"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	dockerpkg "github.com/elasticclaw/elasticclaw/pkg/provider/docker"
	"github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	lambdamicrovms "github.com/elasticclaw/elasticclaw/pkg/provider/lambdamicrovms"
	replicated "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newDaytonaProvider(cfg types.ProviderConfig) (*daytona.Provider, error) {
	return daytona.New(map[string]interface{}{
		"api_key": cfg.APIKey,
		"api_url": cfg.APIURL,
		"target":  cfg.Target,
	})
}

func newExedevProvider(cfg types.ProviderConfig) (*exedev.Provider, error) {
	return exedev.New(exedev.Config{
		SSHKeyPath:    cfg.SSHKeyPath,
		DefaultCPU:    cfg.DefaultCPU,
		DefaultMemory: cfg.DefaultMemory,
		DefaultDisk:   cfg.DefaultDisk,
	})
}

func newDockerProvider(cfg types.ProviderConfig) (*dockerpkg.Provider, error) {
	return dockerpkg.New(dockerpkg.Config{
		Image:   cfg.Image,
		Network: cfg.Network,
	})
}

func newLambdaMicroVMsProvider(cfg types.ProviderConfig) (*lambdamicrovms.Provider, error) {
	return lambdamicrovms.New(lambdamicrovms.Config{
		Region:                   cfg.AWSRegion,
		Profile:                  cfg.AWSProfile,
		ImageIdentifier:          cfg.ImageIdentifier,
		ImageVersion:             cfg.ImageVersion,
		ExecutionRoleARN:         cfg.ExecutionRoleARN,
		IngressNetworkConnectors: cfg.IngressNetworkConnectors,
		EgressNetworkConnectors:  cfg.EgressNetworkConnectors,
		IdleMaxDurationSeconds:   cfg.IdleMaxDurationSeconds,
		SuspendedDurationSeconds: cfg.SuspendedDurationSeconds,
		AutoResume:               cfg.AutoResume,
		MaximumDurationSeconds:   cfg.MaximumDurationSeconds,
		BridgePort:               cfg.MicroVMBridgePort,
		TokenExpirationMinutes:   cfg.AuthTokenExpirationMinutes,
	})
}

func newReplicatedProvider(cfg types.ProviderConfig) (*replicated.Provider, error) {
	return replicated.New(replicated.Config{
		Token:           cfg.Token,
		APIURL:          cfg.APIURL,
		DefaultTTL:      cfg.DefaultTTL,
		DefaultType:     cfg.DefaultInstanceType,
		HubPublicKey:    cfg.SSHPublicKey,
		ExtraPublicKeys: cfg.ExtraSSHPublicKeys,
	})
}

var createDockerContainer = (*dockerpkg.Provider).Create

func (s *Server) provisionWithProvider(ctx context.Context, provider, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	switch provider {
	case "replicated":
		return s.provisionReplicated(ctx, clawID, req, cfg, env)
	case "daytona":
		return s.provisionDaytona(ctx, clawID, req, cfg, files, env)
	case "exedev":
		return s.provisionExedev(ctx, clawID, req, cfg, files, env)
	case "docker":
		return s.provisionDocker(ctx, clawID, req, cfg, files, env)
	case "lambda-microvms":
		return s.provisionLambdaMicroVMs(ctx, clawID, req, cfg, files)
	case "noop":
		if os.Getenv("ELASTICCLAW_NOOP_PROVIDER") == "" {
			return fmt.Errorf("noop provider requires ELASTICCLAW_NOOP_PROVIDER=1 (test use only)")
		}
		providerID := "noop-vm-" + clawID[:8]
		_, _ = s.execStatusLogged("connect claw "+clawID, `UPDATE claws SET status='connected', provider='noop', provider_id=? WHERE id=? AND status NOT IN ('idle','deleted','error')`, providerID, clawID)
		return nil
	default:
		return fmt.Errorf("unsupported provider: %s", provider)
	}
}
