package storage

import "testing"

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
