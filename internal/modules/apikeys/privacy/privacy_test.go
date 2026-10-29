package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testTenant = testID(1)
	testUser   = testID(10)
)

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

type fakeStore struct {
	keys    []Key
	deleted bool
}

func (f *fakeStore) List(_ context.Context, _, _ uuid.UUID, limit int32) ([]Key, bool, error) {
	if limit <= 0 {
		return []Key{}, false, nil
	}
	truncated := len(f.keys) > int(limit)
	keys := f.keys
	if truncated {
		keys = keys[:limit]
	}
	return keys, truncated, nil
}
func (f *fakeStore) DeleteAll(context.Context, uuid.UUID, uuid.UUID) error {
	f.deleted = true
	return nil
}

func TestExportIncludesKeyMetadataButNeverHashes(t *testing.T) {
	store := &fakeStore{keys: []Key{{
		ID: testID(5), Name: "ci", Prefix: "abc", Scopes: []string{"files:read"},
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}}}
	section, err := New(store).Export(context.Background(), testTenant, testUser)
	require.NoError(t, err)
	assert.Equal(t, "apikeys", section.Name)
	var out struct {
		APIKeys []struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			Prefix string    `json:"prefix"`
			Scopes []string  `json:"scopes"`
		} `json:"apiKeys"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.APIKeys, 1)
	assert.Equal(t, testID(5), out.APIKeys[0].ID)
	assert.Equal(t, "ci", out.APIKeys[0].Name)
	assert.Equal(t, []string{"files:read"}, out.APIKeys[0].Scopes)
	assert.NotContains(t, strings.ToLower(string(section.Data)), "hash")
}

func TestEraseDeletesAllUserKeys(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, New(store).Erase(context.Background(), testTenant, testUser))
	assert.True(t, store.deleted)
}

func TestExportHonoursRowLimit(t *testing.T) {
	keys := make([]Key, 3)
	for i := range keys {
		keys[i] = Key{ID: testID(i + 1), Name: "key", ExpiresAt: time.Now(), CreatedAt: time.Now()}
	}
	provider := New(&fakeStore{keys: keys})
	provider.rowLimit = 2
	section, err := provider.Export(context.Background(), testTenant, testUser)
	require.NoError(t, err)
	var out struct {
		APIKeys []json.RawMessage `json:"apiKeys"`
		Budget  struct {
			Truncated bool `json:"truncated"`
		} `json:"budget"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.Len(t, out.APIKeys, 2)
	assert.True(t, section.Truncated)
	assert.True(t, out.Budget.Truncated)
}
