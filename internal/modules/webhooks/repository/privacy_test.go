package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
)

func TestPrivacyStoreDeleteEventsFailsClosedWithoutTransactionManager(t *testing.T) {
	store := webhookrepo.NewPrivacyStore(&fakedb.DB{}, nil)
	err := store.DeleteEvents(context.Background(), testID(1), testID(2))
	require.ErrorIs(t, err, webhookrepo.ErrNoPool)
}
