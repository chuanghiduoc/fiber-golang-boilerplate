//go:build integration

package repository_test

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }
