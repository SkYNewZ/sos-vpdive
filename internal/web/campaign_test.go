package web

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// importCampaign stores campaign_valid.xlsx as the script would push it.
func (e *testEnv) importCampaign(t *testing.T) {
	t.Helper()
	rows, err := xlsx.ReadFirstSheet(fixtureBytes(t, "campaign_valid.xlsx"), imports.Limits())
	require.NoError(t, err)
	exp, err := campaign.Parse(rows, e.srv.paris)
	require.NoError(t, err)
	require.NoError(t, e.deps.Campaign.Import(context.Background(), exp))
}
