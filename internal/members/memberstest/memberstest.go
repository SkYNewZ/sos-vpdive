// Package memberstest holds test helpers for packages that need a members
// list: it must only be imported from tests.
package memberstest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// FixturePath is the absolute path of testdata/fixtures/<name>, whatever the
// package under test.
func FixturePath(name string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("..", "..", "testdata", "fixtures", name) // from internal/<pkg>
	}
	// file is <module>/internal/members/memberstest/memberstest.go
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(root, "testdata", "fixtures", name)
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
	_, err = s.Confirm(ctx, p.ID, "alice", true)
	require.NoError(tb, err)
}
