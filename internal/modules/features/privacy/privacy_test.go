package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

type fakeStore struct {
	targets []Target
	deleted bool
}

func (f *fakeStore) List(context.Context, uuid.UUID, uuid.UUID) ([]Target, error) {
	return f.targets, nil
}
func (f *fakeStore) DeleteAll(context.Context, uuid.UUID, uuid.UUID) error {
	f.deleted = true
	return nil
}

func TestExportIncludesOnlyUserTargets(t *testing.T) {
	store := &fakeStore{targets: []Target{{FlagKey: "new_ui", Enabled: true, CreatedAt: time.Now()}}}
	section, err := New(store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	assert.Equal(t, "features", section.Name)
	var out struct {
		FeatureTargets []struct {
			FlagKey string `json:"flagKey"`
			Enabled bool   `json:"enabled"`
		} `json:"featureTargets"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.FeatureTargets, 1)
	assert.Equal(t, "new_ui", out.FeatureTargets[0].FlagKey)
	assert.True(t, out.FeatureTargets[0].Enabled)
}

func TestEraseDeletesOnlyUserTargets(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, New(store).Erase(context.Background(), testID(1), testID(10)))
	assert.True(t, store.deleted)
}
