-- Privacy export/erasure. Both queries are scoped to the explicit tenant/user
-- passed by the privacy contract, not a request context.

-- name: ListFilesForPrivacyLimited :many
-- Export: one bounded page, newest first. This order matches
-- idx_files_tenant_user_created (tenant_id, user_id, created_at DESC, id DESC).
SELECT * FROM files
WHERE tenant_id = $1 AND user_id = $2
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: DeleteFileForPrivacy :execrows
DELETE FROM files
WHERE id = $1 AND tenant_id = $2 AND user_id = $3;
