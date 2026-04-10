package hub

import (
	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/local"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newDaytonaProvider(cfg types.ProviderConfig) (*daytona.Provider, error) {
	return daytona.New(map[string]interface{}{
		"api_key":  cfg.APIKey,
		"api_url":  cfg.APIURL,
		"target":   cfg.Target,
	})
}

func newLocalProvider() *local.Provider {
	return local.New()
}
