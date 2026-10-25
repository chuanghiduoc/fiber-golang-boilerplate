package service

import (
	"context"
	"log/slog"
	"time"
)

// RetentionResult counts the rows one retention pass deleted.
type RetentionResult struct {
	// Deliveries are terminal outbound deliveries (their attempts cascade).
	Deliveries int64
	// OutboundEvents are webhook_events no remaining delivery references.
	OutboundEvents int64
	// InboundEvents are received inbound events.
	InboundEvents int64
}

// PurgeRetention deletes terminal deliveries, then the outbound events they
// left unreferenced, then old inbound events. Each step runs only after the
// previous one succeeded; the counts of completed steps are returned with the
// error.
func (s *service) PurgeRetention(ctx context.Context, retentionDays int32) (RetentionResult, error) {
	var result RetentionResult
	var err error
	result.Deliveries, err = s.deliveries.PurgeDeliveries(ctx, retentionDays)
	if err != nil {
		return result, err
	}
	result.OutboundEvents, err = s.deliveries.PurgeOrphanEvents(ctx, retentionDays)
	if err != nil {
		return result, err
	}
	result.InboundEvents, err = s.inbound.PurgeInboundEvents(ctx, retentionDays)
	if err != nil {
		return result, err
	}
	return result, nil
}

// RunRetention periodically purges terminal deliveries, unreferenced outbound
// events, and old inbound events. It is best-effort: failures are logged and
// retried on the next tick. The first pass runs immediately so a restarted
// process catches up.
func RunRetention(ctx context.Context, svc Service, retentionDays int32, interval time.Duration) {
	if svc == nil || retentionDays <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	run := func() {
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		result, err := svc.PurgeRetention(runCtx, retentionDays)
		if err != nil {
			slog.Warn("webhooks: retention purge failed",
				slog.Int64("deliveries", result.Deliveries),
				slog.Int64("outbound_events", result.OutboundEvents),
				slog.Int64("inbound_events", result.InboundEvents),
				slog.Any("error", err),
			)
			return
		}
		if result.Deliveries > 0 || result.OutboundEvents > 0 || result.InboundEvents > 0 {
			slog.Info("webhooks: retention purged rows",
				slog.Int64("deliveries", result.Deliveries),
				slog.Int64("outbound_events", result.OutboundEvents),
				slog.Int64("inbound_events", result.InboundEvents),
				slog.Int("retention_days", int(retentionDays)),
			)
		}
	}

	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
