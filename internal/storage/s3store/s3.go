// Package s3store is the S3-compatible storage.Storage driver (AWS S3 and
// the SeaweedFS S3 gateway). It lives apart from pkg/storage so packages that
// only need the interface do not link the AWS SDK.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// Storage driver names backed by Storage.
const (
	DriverS3        = "s3"
	DriverSeaweedFS = "seaweedfs"
)

var _ storage.Storage = (*Storage)(nil)

// Storage stores objects in an S3-compatible bucket.
type Storage struct {
	client   *s3.Client
	bucket   string
	endpoint string
	useSSL   bool
}

// New builds an S3 client for AWS S3 (driver "s3") or the SeaweedFS
// S3 gateway (driver "seaweedfs") and ensures the bucket exists.
//
// Only AWS endpoints (*.amazonaws.com with driver "s3") keep the SDK
// defaults. Everything else, SeaweedFS included, gets path-style addressing
// (S3-compatible servers rarely serve bucket subdomains; this matches the
// previous minio-go auto lookup) and checksums only when an operation
// requires them: since service/s3 v1.73 the SDK adds CRC32 checksums to every
// request, which S3-compatible servers may not implement.
func New(cfg config.StorageConfig) (*Storage, error) {
	scheme := "http"
	if cfg.S3UseSSL {
		scheme = "https"
	}
	compatible := cfg.Driver == DriverSeaweedFS || !isAWSEndpoint(cfg.S3Endpoint)

	client := s3.New(s3.Options{
		Region:       cfg.S3Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		BaseEndpoint: aws.String(scheme + "://" + cfg.S3Endpoint),
		UsePathStyle: compatible,
	}, func(o *s3.Options) {
		if compatible {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})

	// Ensure bucket exists. Bound startup so an unreachable endpoint cannot
	// hang the process indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureBucket(ctx, client, cfg.S3Bucket, cfg.S3Region); err != nil {
		return nil, err
	}

	return &Storage{
		client:   client,
		bucket:   cfg.S3Bucket,
		endpoint: cfg.S3Endpoint,
		useSSL:   cfg.S3UseSSL,
	}, nil
}

// isAWSEndpoint reports whether endpoint (host[:port], no scheme) is an AWS
// S3 host.
func isAWSEndpoint(endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	return strings.HasSuffix(strings.ToLower(host), ".amazonaws.com")
}

func ensureBucket(ctx context.Context, client *s3.Client, bucket, region string) error {
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	var notFound *types.NotFound
	if !errors.As(err, &notFound) {
		return fmt.Errorf("failed to check bucket: %w", err)
	}

	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	// us-east-1 is the default location and must not be sent as a constraint.
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}
	if _, err := client.CreateBucket(ctx, input); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) {
			return nil
		}
		return fmt.Errorf("failed to create bucket: %w", err)
	}
	return nil
}

func (s *Storage) Put(ctx context.Context, path string, reader io.Reader, size int64, contentType string) error {
	// The SDK signs the payload hash, which needs a seekable body over plain
	// HTTP. Multipart uploads are already seekable; other readers are buffered,
	// bounded by the caller-supplied size.
	body, ok := reader.(io.ReadSeeker)
	if !ok {
		buf := bytes.NewBuffer(make([]byte, 0, size))
		if _, err := io.CopyN(buf, reader, size); err != nil {
			return fmt.Errorf("failed to read upload body: %w", err)
		}
		body = bytes.NewReader(buf.Bytes())
	}

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(path),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

func (s *Storage) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	// The response body is streamed after the handler returns, so the request
	// deadline is no longer suitable. Bound the stream independently and cancel
	// it when the caller closes the body. GetObject fails before any body is
	// returned when the object is missing, so the handler never sends a 200
	// for a missing blob.
	streamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	obj, err := s.client.GetObject(streamCtx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		cancel()
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, path)
		}
		return nil, fmt.Errorf("failed to get from S3: %w", err)
	}

	return &cancelReadCloser{ReadCloser: obj.Body, cancel: cancel}, nil
}

// s3NoSuchBucketCode is the S3 error code for a missing bucket.
const s3NoSuchBucketCode = "NoSuchBucket"

// isS3NotFound reports a missing object: the typed NoSuchKey error, or any
// HTTP 404 response (some S3-compatible servers omit the typed error code).
// A missing bucket is also a 404 but is a configuration error, not a missing
// blob, so it stays an internal error instead of reading as data loss.
func isS3NotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == s3NoSuchBucketCode {
		return false
	}
	var status interface{ HTTPStatusCode() int }
	return errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound
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

func (s *Storage) Delete(ctx context.Context, path string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	}); err != nil {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}

	return nil
}

// List returns every object under prefix with its size and last write time.
func (s *Storage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	var objects []storage.Object
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list S3 objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			info := storage.Object{Path: *object.Key}
			if object.Size != nil {
				info.Size = *object.Size
			}
			if object.LastModified != nil {
				info.ModifiedAt = object.LastModified.UTC()
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

func (s *Storage) URL(path string) string {
	scheme := "http"
	if s.useSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, s.endpoint, s.bucket, path)
}
