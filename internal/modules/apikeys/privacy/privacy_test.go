package privacy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	keys    []Key
	deleted bool
}

func (f *fakeStore) List(context.Context, int64, int64) ([]Key, error) { return f.keys, nil }
func (f *fakeStore) DeleteAll(context.Context, int64, int64) error     { f.deleted = true; return nil }

func TestExportIncludesKeyMetadataButNeverHashes(t *testing.T) {
	store := &fakeStore{keys: []Key{{
		ID: 1, Name: "ci", Prefix: "abc", Scopes: []string{"files:read"},
		CreatedAt: time.Now(),
	}}}
	section, err := New(store).Export(context.Background(), 1, 10)
	require.NoError(t, err)
	assert.Equal(t, "apikeys", section.Name)
	var out struct {
		APIKeys []struct {
			Name   string   `json:"name"`
			Prefix string   `json:"prefix"`
			Scopes []string `json:"scopes"`
		} `json:"apiKeys"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.APIKeys, 1)
	assert.Equal(t, "ci", out.APIKeys[0].Name)
	assert.Equal(t, []string{"files:read"}, out.APIKeys[0].Scopes)
	assert.NotContains(t, strings.ToLower(string(section.Data)), "hash")
}

func TestEraseDeletesAllUserKeys(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, New(store).Erase(context.Background(), 1, 10))
	assert.True(t, store.deleted)
}
