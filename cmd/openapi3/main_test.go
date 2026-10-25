package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertRepositorySpec(t *testing.T) {
	raw, err := os.ReadFile("../../docs/swagger.json")
	require.NoError(t, err)

	converted, err := convert(raw)
	require.NoError(t, err)

	var doc struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(converted, &doc))
	require.Equal(t, "3.0.3", doc.OpenAPI)
	require.NotEmpty(t, doc.Paths)
	require.NotEmpty(t, doc.Servers)
	require.Equal(t, "/api/v1", doc.Servers[0].URL)
}

func TestConvertRejectsNonSwagger2(t *testing.T) {
	_, err := convert([]byte(`{"openapi":"3.0.3","paths":{}}`))
	require.Error(t, err)

	_, err = convert([]byte(`not json`))
	require.Error(t, err)
}
