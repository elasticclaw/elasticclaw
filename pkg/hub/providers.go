package hub

import (
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	dockerpkg "github.com/elasticclaw/elasticclaw/pkg/provider/docker"
	"github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	lambdamicrovms "github.com/elasticclaw/elasticclaw/pkg/provider/lambdamicrovms"
	replicated "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func previewPorts(port int) []int {
	if port == 0 {
		return nil
	}
	return []int{port}
}

func instancePreviewURL(instance *types.Instance, port int) string {
	if instance == nil || port == 0 {
		return ""
	}
	return instance.ProviderMeta[fmt.Sprintf("preview_url_%d", port)]
}

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
