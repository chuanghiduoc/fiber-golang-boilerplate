-- Privacy export/erasure. Both queries are scoped to the explicit tenant/user
-- passed by the privacy contract, not a request context.

-- name: ListFilesForPrivacy :many
SELECT * FROM files
WHERE tenant_id = $1 AND user_id = $2
ORDER BY id;

-- name: DeleteFileForPrivacy :execrows
DELETE FROM files
WHERE id = $1 AND tenant_id = $2 AND user_id = $3;
