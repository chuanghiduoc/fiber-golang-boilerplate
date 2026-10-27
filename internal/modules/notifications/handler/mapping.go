package handler

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
)

func toNotificationResponse(n *service.Notification) *dto.NotificationResponse {
	if n == nil {
		return nil
	}
	return &dto.NotificationResponse{
		ID:        n.ID,
		TenantID:  n.TenantID,
		UserID:    n.UserID,
		Type:      n.Type,
		Title:     n.Title,
		Body:      n.Body,
		Data:      n.Data,
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

func toNotificationResponses(items []service.Notification) []dto.NotificationResponse {
	out := make([]dto.NotificationResponse, len(items))
	for i := range items {
		out[i] = *toNotificationResponse(&items[i])
	}
	return out
}

func toPreferenceResponse(p *service.Preference) *dto.NotificationPreferenceResponse {
	if p == nil {
		return nil
	}
	return &dto.NotificationPreferenceResponse{
		Type:      p.Type,
		Channel:   p.Channel,
		Enabled:   p.Enabled,
		UpdatedAt: p.UpdatedAt,
	}
}

func toPreferenceResponses(items []service.Preference) []dto.NotificationPreferenceResponse {
	out := make([]dto.NotificationPreferenceResponse, len(items))
	for i := range items {
		out[i] = *toPreferenceResponse(&items[i])
	}
	return out
}
