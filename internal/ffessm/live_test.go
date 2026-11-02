package ffessm

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// TestLiveSearch signs in to the real portal with the club account of the
// repository's .env and runs FFESSM_LIVE_QUERY. It prints only counts:
// results are personal data.
//
//	FFESSM_LIVE=1 FFESSM_LIVE_QUERY=A-01-000001 go test -run TestLiveSearch -v ./internal/ffessm
func TestLiveSearch(t *testing.T) {
	if os.Getenv("FFESSM_LIVE") == "" {
		t.Skip("set FFESSM_LIVE=1 to sign in to the real portal")
	}
	query := os.Getenv("FFESSM_LIVE_QUERY")
	require.NotEmpty(t, query, "FFESSM_LIVE_QUERY")
	env := dotEnv(t, "../../.env")
	c := New(&config.FFESSM{Username: env["FFESSM_USERNAME"], Password: env["FFESSM_PASSWORD"]})

	got, err := c.Search(context.Background(), query)
	require.NoError(t, err)
	active := 0
	for _, l := range got {
		assert.NotEmpty(t, l.Number)
		if l.Active {
			active++
		}
	}
	t.Logf("%d licence(s), %d active", len(got), active)
	assert.NotEmpty(t, got)

	first, expiry := c.token, c.expiry
	assert.WithinDuration(t, time.Now().Add(time.Hour-tokenMargin), expiry, time.Minute, "token kept for its lifetime minus the margin")
	_, err = c.Search(context.Background(), query)
	require.NoError(t, err)
	assert.Equal(t, first, c.token, "second search reuses the token")

	c.token = "refused" // the portal answers 401: the client must sign in again
	again, err := c.Search(context.Background(), query)
	require.NoError(t, err)
	assert.Len(t, again, len(got))
	assert.NotEqual(t, "refused", c.token)
	t.Log("token reused, then renewed after a 401")
}

// dotEnv reads the FFESSM_ variables of an env file, unquoted.
func dotEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	env := map[string]string{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		key, value, ok := strings.Cut(lines.Text(), "=")
		if ok && strings.HasPrefix(key, "FFESSM_") {
			env[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	require.NoError(t, lines.Err())
	require.NotEmpty(t, env["FFESSM_USERNAME"], "FFESSM_USERNAME in .env")
	return env
}
