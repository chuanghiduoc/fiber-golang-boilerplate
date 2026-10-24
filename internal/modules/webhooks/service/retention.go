package service

import (
	"context"
	"log/slog"
	"time"
)

// PurgeRetention deletes terminal deliveries and then old inbound events. A
// failure in the second step still reports the deliveries already purged.
func (s *service) PurgeRetention(ctx context.Context, retentionDays int32) (deliveries, events int64, err error) {
	deliveries, err = s.deliveries.PurgeDeliveries(ctx, retentionDays)
	if err != nil {
		return 0, 0, err
	}
	events, err = s.inbound.PurgeInboundEvents(ctx, retentionDays)
	if err != nil {
		return deliveries, 0, err
	}
	return deliveries, events, nil
}

// RunRetention periodically purges terminal deliveries and old inbound events.
// It is best-effort: failures are logged and retried on the next tick. The
// first pass runs immediately so a restarted process catches up.
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
		deliveries, events, err := svc.PurgeRetention(runCtx, retentionDays)
		if err != nil {
			slog.Warn("webhooks: retention purge failed",
				slog.Int64("deliveries", deliveries),
				slog.Int64("events", events),
				slog.Any("error", err),
			)
			return
		}
		if deliveries > 0 || events > 0 {
			slog.Info("webhooks: retention purged rows",
				slog.Int64("deliveries", deliveries),
				slog.Int64("inbound_events", events),
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
