// Package memberstest holds test helpers for packages that need a members
// list: it must only be imported from tests.
package memberstest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// FixturePath is the path of testdata/fixtures/<name> from internal/<pkg>,
// where every importer lives.
func FixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", name)
}

// Import reads a fixture and replaces the members list with it, as alice.
func Import(tb testing.TB, s *members.Store, name string) {
	tb.Helper()
	data, err := os.ReadFile(FixturePath(name))
	require.NoError(tb, err)
	rows, err := xlsx.ReadFirstSheet(data, members.ImportLimits())
	require.NoError(tb, err)
	exp, err := members.Parse(rows, time.UTC)
	require.NoError(tb, err)
	ctx := context.Background()
	p, err := s.NewPreview(ctx, "alice", exp)
	require.NoError(tb, err)
	require.NoError(tb, s.Confirm(ctx, p.ID, "alice", true))
}
