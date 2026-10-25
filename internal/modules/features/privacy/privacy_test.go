package privacy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	targets []Target
	deleted bool
}

func (f *fakeStore) List(context.Context, int64, int64) ([]Target, error) { return f.targets, nil }
func (f *fakeStore) DeleteAll(context.Context, int64, int64) error        { f.deleted = true; return nil }

func TestExportIncludesOnlyUserTargets(t *testing.T) {
	store := &fakeStore{targets: []Target{{FlagKey: "new_ui", Enabled: true, CreatedAt: time.Now()}}}
	section, err := New(store).Export(context.Background(), 1, 10)
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
	require.NoError(t, New(store).Erase(context.Background(), 1, 10))
	assert.True(t, store.deleted)
}
