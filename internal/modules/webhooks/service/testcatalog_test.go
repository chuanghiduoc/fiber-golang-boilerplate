package service

import (
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
)

func init() {
	if err := webhookscatalog.Register(); err != nil {
		panic(err)
	}
}
