package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Config struct {
	Bucket          string
	Region          string
	Endpoint        string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	PathStyle       bool
	HTTPClient      aws.HTTPClient
}

type S3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("s3 artifact bucket is required")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	loadOpts := []func(*awscfg.LoadOptions) error{awscfg.WithRegion(region)}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" || cfg.SessionToken != "" {
		loadOpts = append(loadOpts, awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)))
	}
	awsCfg, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.HTTPClient != nil {
			o.HTTPClient = cfg.HTTPClient
		}
		if strings.TrimSpace(cfg.Endpoint) != "" {
			endpoint := strings.TrimSpace(cfg.Endpoint)
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &S3Store{
		client: client,
		bucket: strings.TrimSpace(cfg.Bucket),
		prefix: cleanPrefix(cfg.Prefix),
	}, nil
}

func (s *S3Store) PutBlob(ctx context.Context, r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp("", "elasticclaw-artifact-blob-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(r, h))
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	key, err := s.blobKey(digest)
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	defer tmp.Close()

	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          tmp,
		ContentLength: aws.Int64(size),
	}); err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

func (s *S3Store) GetBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	key, err := s.blobKey(digest)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return newVerifyingReadCloser(out.Body, digest), nil
}

func (s *S3Store) PutManifest(ctx context.Context, manifest []byte) (string, error) {
	digest := DigestBytes(manifest)
	key, err := s.manifestKey(digest)
	if err != nil {
		return "", err
	}
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(manifest),
		ContentType: aws.String("application/vnd.oci.image.manifest.v1+json"),
	}); err != nil {
		return "", err
	}
	return digest, nil
}

func (s *S3Store) GetManifest(ctx context.Context, digest string) ([]byte, error) {
	key, err := s.manifestKey(digest)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	if got := DigestBytes(data); got != digest {
		return nil, fmt.Errorf("artifact manifest digest mismatch: got %s, want %s", got, digest)
	}
	return data, nil
}

func (s *S3Store) ResolveRef(ctx context.Context, repo, tag string) (string, error) {
	key, err := s.refKey(repo, tag)
	if err != nil {
		return "", err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(data))
	if _, _, err := ParseDigest(digest); err != nil {
		return "", fmt.Errorf("invalid digest in ref %s:%s: %w", repo, tag, err)
	}
	return digest, nil
}

func (s *S3Store) Tag(ctx context.Context, repo, tag, digest string) error {
	if _, _, err := ParseDigest(digest); err != nil {
		return err
	}
	key, err := s.refKey(repo, tag)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(digest + "\n"),
	})
	return err
}

func (s *S3Store) blobKey(digest string) (string, error) {
	_, encoded, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	return s.key("blobs", "sha256", encoded[:2], encoded[2:4], encoded), nil
}

func (s *S3Store) manifestKey(digest string) (string, error) {
	_, encoded, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	return s.key("manifests", "sha256", encoded[:2], encoded[2:4], encoded), nil
}

func (s *S3Store) refKey(repo, tag string) (string, error) {
	if err := ValidateRef(repo, tag); err != nil {
		return "", err
	}
	parts := append([]string{"refs"}, strings.Split(repo, "/")...)
	parts = append(parts, tag)
	return s.key(parts...), nil
}

func (s *S3Store) key(parts ...string) string {
	if s.prefix == "" {
		return path.Join(parts...)
	}
	all := append([]string{s.prefix}, parts...)
	return path.Join(all...)
}

func cleanPrefix(prefix string) string {
	prefix = strings.TrimSpace(strings.Trim(prefix, "/"))
	if prefix == "." {
		return ""
	}
	return prefix
}
