package s3store

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type statusError int

func (e statusError) Error() string       { return "http status" }
func (e statusError) HTTPStatusCode() int { return int(e) }

// codedStatusError is an S3 API error with both an error code and a status.
type codedStatusError struct {
	code   string
	status int
}

func (e codedStatusError) Error() string       { return e.code }
func (e codedStatusError) ErrorCode() string   { return e.code }
func (e codedStatusError) HTTPStatusCode() int { return e.status }

func TestIsS3NotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NoSuchKey", fmt.Errorf("get: %w", &types.NoSuchKey{}), true},
		{"plain 404 response", fmt.Errorf("get: %w", statusError(http.StatusNotFound)), true},
		{"missing bucket is a configuration error", fmt.Errorf("get: %w", codedStatusError{"NoSuchBucket", http.StatusNotFound}), false},
		{"typed NoSuchBucket", fmt.Errorf("get: %w", &types.NoSuchBucket{}), false},
		{"403 response", fmt.Errorf("get: %w", statusError(http.StatusForbidden)), false},
		{"transport error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		if got := isS3NotFound(tc.err); got != tc.want {
			t.Errorf("%s: isS3NotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsAWSEndpoint(t *testing.T) {
	cases := map[string]bool{
		"s3.us-east-1.amazonaws.com":     true,
		"s3.amazonaws.com:443":           true,
		"S3.EU-WEST-1.AMAZONAWS.COM":     true,
		"seaweedfs:8333":                 false,
		"127.0.0.1:8333":                 false,
		"abc.r2.cloudflarestorage.com":   false,
		"amazonaws.com.attacker.example": false,
	}
	for endpoint, want := range cases {
		if got := isAWSEndpoint(endpoint); got != want {
			t.Errorf("isAWSEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}
