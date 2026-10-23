package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type S3Storage struct {
	client   *minio.Client
	bucket   string
	endpoint string
	useSSL   bool
}

func NewS3Storage(cfg config.StorageConfig) (*S3Storage, error) {
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Ensure bucket exists. Bound startup so an unreachable endpoint cannot
	// hang the process indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, cfg.S3Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.S3Bucket, minio.MakeBucketOptions{Region: cfg.S3Region}); err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
	}

	return &S3Storage{
		client:   client,
		bucket:   cfg.S3Bucket,
		endpoint: cfg.S3Endpoint,
		useSSL:   cfg.S3UseSSL,
	}, nil
}

func (s *S3Storage) Put(ctx context.Context, path string, reader io.Reader, size int64, contentType string) error {
	opts := minio.PutObjectOptions{
		ContentType: contentType,
	}

	_, err := s.client.PutObject(ctx, s.bucket, path, reader, size, opts)
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

func (s *S3Storage) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	// GetObject is lazy; a missing object would otherwise fail after the HTTP
	// handler has already sent its 200 response. Stat first so failures are
	// reported as an error.
	if _, err := s.client.StatObject(ctx, s.bucket, path, minio.StatObjectOptions{}); err != nil {
		return nil, fmt.Errorf("failed to stat S3 object: %w", err)
	}

	// The response body is streamed after the handler returns, so the request
	// deadline is no longer suitable. Bound the stream independently and cancel
	// it when the caller closes the body.
	streamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	obj, err := s.client.GetObject(streamCtx, s.bucket, path, minio.GetObjectOptions{})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to get from S3: %w", err)
	}

	return &cancelReadCloser{ReadCloser: obj, cancel: cancel}, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *S3Storage) Delete(ctx context.Context, path string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, path, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}

	return nil
}

func (s *S3Storage) URL(path string) string {
	scheme := "http"
	if s.useSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, s.endpoint, s.bucket, path)
}
