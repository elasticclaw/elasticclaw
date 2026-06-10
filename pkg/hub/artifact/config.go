package artifact

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func NewStoreFromHubConfig(ctx context.Context, dataDir string, cfg *types.ArtifactStorageConfig) (Store, error) {
	backend := "local"
	if cfg != nil && strings.TrimSpace(cfg.Backend) != "" {
		backend = strings.ToLower(strings.TrimSpace(cfg.Backend))
	}
	switch backend {
	case "local":
		root := filepath.Join(dataDir, "artifacts")
		if cfg != nil && cfg.Local != nil && strings.TrimSpace(cfg.Local.Path) != "" {
			root = cfg.Local.Path
		}
		return NewLocalStore(root)
	case "s3":
		if cfg == nil || cfg.S3 == nil {
			return nil, fmt.Errorf("artifact_storage.s3 is required when backend is s3")
		}
		return NewS3Store(ctx, S3Config{
			Bucket:          cfg.S3.Bucket,
			Region:          cfg.S3.Region,
			Endpoint:        cfg.S3.Endpoint,
			Prefix:          cfg.S3.Prefix,
			AccessKeyID:     cfg.S3.AccessKeyID,
			SecretAccessKey: cfg.S3.SecretAccessKey,
			SessionToken:    cfg.S3.SessionToken,
			PathStyle:       cfg.S3.PathStyle,
		})
	default:
		return nil, fmt.Errorf("unsupported artifact storage backend %q", backend)
	}
}
