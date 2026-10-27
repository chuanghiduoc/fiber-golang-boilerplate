//go:build integration

package authn_test

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }
