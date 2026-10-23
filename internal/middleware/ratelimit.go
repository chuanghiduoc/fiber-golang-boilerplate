package middleware

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

func NewLimiter(maxRequests, windowSecs int, prefix string, storages ...fiber.Storage) fiber.Handler {
	cfg := limiter.Config{
		Max:        maxRequests,
		Expiration: time.Duration(windowSecs) * time.Second,
		KeyGenerator: func(c fiber.Ctx) string {
			// Namespace by tier (strict/normal/relaxed) so one bucket cannot
			// consume another tier's quota. Authenticated routes are limited per
			// user so clients behind a shared NAT do not exhaust each other.
			if id := fiber.Locals[int64](c, "user_id"); id != 0 {
				return prefix + ":user:" + strconv.FormatInt(id, 10)
			}
			return prefix + ":ip:" + c.IP()
		},
		LimitReached: func(c fiber.Ctx) error {
			return fiber.NewError(fiber.StatusTooManyRequests, "too many requests, please try again later")
		},
	}
	if len(storages) > 0 && storages[0] != nil {
		cfg.Storage = storages[0]
	}
	return limiter.New(cfg)
}
