package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by webhooks.
const (
	// CodeWebhookPortNotAllowed: the endpoint URL uses a destination port
	// outside WEBHOOK_ALLOWED_PORTS (which always includes 443 and 80).
	CodeWebhookPortNotAllowed apperror.Code = "webhook.port_not_allowed"
)

// ErrorCodes returns the webhooks error catalog for app.ErrorProvider.
func ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{
		CodeWebhookPortNotAllowed: {
			Status:  http.StatusBadRequest,
			Title:   "Webhook destination port is not allowed",
			DocPath: "/errors/webhook-port-not-allowed",
		},
	}
}
