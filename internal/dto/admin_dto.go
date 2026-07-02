package dto

type UpdateRoleRequest struct {
	Role string `json:"role" validate:"required,oneof=user admin"`
}

type AdminStatsResponse struct {
	ActiveUsers   int64 `json:"activeUsers"`
	DeletedUsers  int64 `json:"deletedUsers"`
	TotalFiles    int64 `json:"totalFiles"`
	TotalFileSize int64 `json:"totalFileSize"`
}
