package hub

import (
	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	"github.com/elasticclaw/elasticclaw/pkg/provider/local"
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

func newLocalProvider() *local.Provider {
	return local.New()
}

func newExedevProvider(cfg types.ProviderConfig) (*exedev.Provider, error) {
	return exedev.New(exedev.Config{
		SSHKeyPath: cfg.SSHKeyPath,
	})
}

func newReplicatedProvider(cfg types.ProviderConfig) (*replicated.Provider, error) {
	return replicated.New(replicated.Config{
		Token:             cfg.Token,
		APIURL:            cfg.APIURL,
		DefaultTTL:        cfg.DefaultTTL,
		DefaultType:       cfg.DefaultInstanceType,
		HubPublicKey:      cfg.SSHPublicKey,
		ExtraPublicKeys:   cfg.ExtraSSHPublicKeys,
	})
}
