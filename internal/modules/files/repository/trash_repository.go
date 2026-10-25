package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TrashRepository implements service.TrashRepository. It is the only files
// repository that does not scope queries to the request tenant: the purge is
// a cross-tenant maintenance pass (see queries/trash.sql).
type TrashRepository struct {
	txManager *database.TxManager
}

var _ filesvc.TrashRepository = (*TrashRepository)(nil)

func NewTrashRepository(txManager *database.TxManager) *TrashRepository {
	return &TrashRepository{txManager: txManager}
}

func (r *TrashRepository) PurgeBatch(ctx context.Context, retentionDays, limit int32, purge filesvc.TrashPurgeFunc) (int64, error) {
	var deleted int64
	err := r.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		q := filesqlc.New(tx)
		rows, err := q.LockTrashedFiles(ctx, filesqlc.LockTrashedFilesParams{
			RetentionDays: retentionDays,
			BatchSize:     limit,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		batch := make([]filesvc.TrashedFile, len(rows))
		for i, row := range rows {
			batch[i] = filesvc.TrashedFile{ID: row.ID, TenantID: row.TenantID, StoragePath: row.StoragePath}
		}
		ids, err := purge(ctx, batch)
		if err != nil || len(ids) == 0 {
			return err
		}
		deleted, err = q.HardDeleteTrashedFiles(ctx, ids)
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
