// Package minio stores attachment bodies in an S3-compatible bucket.
package minio

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

type Storage struct {
	logger *zap.Logger
	cfg    *Config
	client *minio.Client
}

func New(ctx context.Context, logger *zap.Logger, cfg *Config) (*Storage, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create minio client: %w", err)
	}
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
		logger.Info("bucket created", zap.String("bucket", cfg.Bucket))
	}
	logger.Info("connected to object storage", zap.String("endpoint", cfg.Endpoint), zap.String("bucket", cfg.Bucket))
	return &Storage{logger: logger, cfg: cfg, client: client}, nil
}

// Put streams r under key and returns the number of bytes stored.
func (s *Storage) Put(ctx context.Context, key string, r io.Reader, contentType string) (int64, error) {
	info, err := s.client.PutObject(ctx, s.cfg.Bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    s.cfg.PartSize,
	})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
}

// GetRange reads length bytes of key from offset on, or with length 0 the
// rest of it.
func (s *Storage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	var opts minio.GetObjectOptions
	if offset > 0 || length > 0 {
		end := int64(0)
		if length > 0 {
			end = offset + length - 1
		}
		if err := opts.SetRange(offset, end); err != nil {
			return nil, err
		}
	}
	return s.client.GetObject(ctx, s.cfg.Bucket, key, opts)
}

func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{})
}

func (s *Storage) Ping(ctx context.Context) error {
	_, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	return err
}
