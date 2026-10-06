// Package paymentstest holds test helpers for packages that need payment
// lines: it must only be imported from tests.
package paymentstest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// Import reads testdata/fixtures/<name> and replaces the payment lines with
// it, as alice.
func Import(tb testing.TB, s *payments.Store, name string) {
	tb.Helper()
	data, err := os.ReadFile(memberstest.FixturePath(name))
	require.NoError(tb, err)
	ImportBytes(tb, s, data)
}

// ImportBytes replaces the payment lines with the workbook data, as alice.
// Dates are read in Paris time.
func ImportBytes(tb testing.TB, s *payments.Store, data []byte) {
	tb.Helper()
	rows, err := xlsx.ReadFirstSheet(data, imports.Limits())
	require.NoError(tb, err)
	created, _ := xlsx.Created(data, imports.Limits())
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(tb, err)
	exp, err := payments.Parse(rows, created, paris)
	require.NoError(tb, err)
	ctx := context.Background()
	p, err := s.NewPreview(ctx, "alice", exp)
	require.NoError(tb, err)
	require.NoError(tb, s.Confirm(ctx, p.ID, "alice", true))
}
