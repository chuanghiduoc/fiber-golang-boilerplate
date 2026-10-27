//go:build integration

package scheduler

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }
