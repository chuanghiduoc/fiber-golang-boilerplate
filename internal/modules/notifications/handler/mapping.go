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

func toPreferenceResponse(p *service.Preference) *dto.PreferenceResponse {
	if p == nil {
		return nil
	}
	return &dto.PreferenceResponse{
		ID:        p.ID,
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		Type:      p.Type,
		Channel:   p.Channel,
		Enabled:   p.Enabled,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func toPreferenceResponses(items []service.Preference) []dto.PreferenceResponse {
	out := make([]dto.PreferenceResponse, len(items))
	for i := range items {
		out[i] = *toPreferenceResponse(&items[i])
	}
	return out
}

func toPreferenceUpdate(req dto.UpdatePreferenceRequest) service.PreferenceUpdate {
	return service.PreferenceUpdate{Type: req.Type, Channel: req.Channel, Enabled: req.Enabled}
}

func toSendNotificationResponse(result service.CreateResult) dto.SendNotificationResponse {
	return dto.SendNotificationResponse{
		Notification: toNotificationResponse(result.Notification),
		EmailQueued:  result.EmailQueued,
	}
}
