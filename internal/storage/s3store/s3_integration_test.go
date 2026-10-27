//go:build integration

package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const (
	seaweedImage     = "chrislusf/seaweedfs:4.48"
	seaweedAccessKey = "testkey"
	seaweedSecretKey = "testsecret"
	seaweedS3Config  = `{"identities":[{"name":"app","credentials":[{"accessKey":"testkey","secretKey":"testsecret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`
)

// seaweedEndpoint returns a SeaweedFS S3 endpoint (host:port). Set
// TEST_SEAWEEDFS_ENDPOINT to reuse a running gateway that accepts the test
// credentials; otherwise a disposable container is started.
func seaweedEndpoint(t *testing.T) string {
	t.Helper()
	if endpoint := os.Getenv("TEST_SEAWEEDFS_ENDPOINT"); endpoint != "" {
		return endpoint
	}

	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        seaweedImage,
			ExposedPorts: []string{"8333/tcp"},
			Cmd:          []string{"server", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-dir=/data"},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(seaweedS3Config),
				ContainerFilePath: "/etc/seaweedfs/s3.json",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForLog("Start Seaweed S3 API Server").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start seaweedfs: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	endpoint, err := container.PortEndpoint(ctx, "8333/tcp", "")
	if err != nil {
		t.Fatalf("seaweedfs endpoint: %v", err)
	}
	return endpoint
}

// newSeaweedStorage retries because the S3 gateway logs readiness slightly
// before the filer accepts bucket operations.
func newSeaweedStorage(t *testing.T, endpoint, bucket string) *Storage {
	t.Helper()
	cfg := config.StorageConfig{
		Driver:      DriverSeaweedFS,
		S3Endpoint:  endpoint,
		S3Region:    "us-east-1",
		S3Bucket:    bucket,
		S3AccessKey: seaweedAccessKey,
		S3SecretKey: seaweedSecretKey,
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		store, err := New(cfg)
		if err == nil {
			return store
		}
		if time.Now().After(deadline) {
			t.Fatalf("New: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func TestSeaweedFSStorageRoundTrip(t *testing.T) {
	endpoint := seaweedEndpoint(t)
	ctx := context.Background()

	// The first construction creates the bucket; a second one must accept the
	// existing bucket.
	newSeaweedStorage(t, endpoint, "uploads-it")
	store := newSeaweedStorage(t, endpoint, "uploads-it")

	payload := []byte("hello seaweedfs")
	// io.MultiReader is not seekable, exercising the buffered Put path; the
	// bytes.Reader case exercises the direct seekable path.
	readers := map[string]io.Reader{
		"seekable":     bytes.NewReader(payload),
		"non-seekable": io.MultiReader(bytes.NewReader(payload)),
	}
	for name, reader := range readers {
		t.Run(name, func(t *testing.T) {
			key := "1/1/" + strings.ReplaceAll(name, "-", "_") + ".txt"
			if err := store.Put(ctx, key, reader, int64(len(payload)), "text/plain"); err != nil {
				t.Fatalf("Put: %v", err)
			}

			body, err := store.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("Get = %q, want %q", got, payload)
			}

			if err := store.Delete(ctx, key); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := store.Get(ctx, key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("Get after Delete error = %v, want storage.ErrNotFound", err)
			}
			// Deleting a missing object succeeds: the files trash purge
			// relies on it when a retry finds the blob already gone.
			if err := store.Delete(ctx, key); err != nil {
				t.Fatalf("Delete of a missing object: %v", err)
			}
		})
	}
}

func TestSeaweedFSStorageList(t *testing.T) {
	endpoint := seaweedEndpoint(t)
	ctx := context.Background()
	store := newSeaweedStorage(t, endpoint, "uploads-list-it")
	bucketPrefix := "list-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-") + "/"
	for _, key := range []string{bucketPrefix + "1/2/a.txt", bucketPrefix + "1/2/nested/b.txt", bucketPrefix + "12/3/c.txt"} {
		if err := store.Put(ctx, key, bytes.NewReader([]byte(key)), int64(len(key)), "text/plain"); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}
	objects, err := store.List(ctx, bucketPrefix+"1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make(map[string]bool, len(objects))
	for _, object := range objects {
		got[object.Path] = true
		if object.ModifiedAt.IsZero() {
			t.Errorf("object %q has no last-modified time", object.Path)
		}
	}
	if len(got) != 2 || !got[bucketPrefix+"1/2/a.txt"] || !got[bucketPrefix+"1/2/nested/b.txt"] {
		t.Fatalf("List = %v, want the two objects under the prefix", got)
	}
}

func TestSeaweedFSStorageRejectsBadCredentials(t *testing.T) {
	endpoint := seaweedEndpoint(t)
	_, err := New(config.StorageConfig{
		Driver:      DriverSeaweedFS,
		S3Endpoint:  endpoint,
		S3Region:    "us-east-1",
		S3Bucket:    "uploads-denied",
		S3AccessKey: seaweedAccessKey,
		S3SecretKey: "wrong-secret",
	})
	if err == nil {
		t.Fatal("New should fail with an invalid secret key")
	}
}
