package tickets

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// importCampaign stores the people of campaign_valid.xlsx that keep accepts.
func (e *env) importCampaign(t *testing.T, keep func(campaign.Person) bool) {
	t.Helper()
	workbook, err := os.ReadFile(memberstest.FixturePath("campaign_valid.xlsx"))
	require.NoError(t, err)
	rows, err := xlsx.ReadFirstSheet(workbook, imports.Limits())
	require.NoError(t, err)
	exp, err := campaign.Parse(rows, time.UTC)
	require.NoError(t, err)
	exp.People = slices.DeleteFunc(exp.People, func(p campaign.Person) bool { return !keep(p) })
	require.NoError(t, e.store.Campaign.Import(context.Background(), exp))
}

// Design 2026-10-20 « Mail d'accusé de réception »: the acknowledgement adds
// one line when the four conditions hold, and is the usual mail otherwise.
func TestAcknowledgementSaysTheMembershipWaitsForTheClub(t *testing.T) {
	const line = "Ton dossier d'adhésion est en attente de validation de la part du club : c'est ce qui bloque le paiement."
	everyone := func(campaign.Person) bool { return true }
	cases := []struct {
		name, email, first, last, category string
		keep                               func(campaign.Person) bool
		want                               bool
	}{
		{"all four hold", "hugo.bernard@example.org", "Hugo", "Bernard", "adhesion", everyone, true},
		{"another category", "hugo.bernard@example.org", "Hugo", "Bernard", "carnet", everyone, false},
		{"not in the members list, never by the typed name", "nouveau@example.org", "Hugo", "Bernard", "adhesion", everyone, false},
		{"no campaign line for the member", "hugo.bernard@example.org", "Hugo", "Bernard", "adhesion",
			func(p campaign.Person) bool { return p.LastName != "Bernard" }, false},
		{"homonyms", "lea.martin@example.org", "Léa", "Martin", "adhesion", everyone, false},
		{"already validated", "chloe.petit@example.org", "Chloé", "Petit", "adhesion", everyone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestStore(t)
			memberstest.Import(t, e.members, "members_valid.xlsx")
			e.importCampaign(t, c.keep)
			sub := submission(t)
			sub.Email, sub.FirstName, sub.LastName, sub.Fields.Category = c.email, c.first, c.last, c.category
			_, err := e.store.Submit(context.Background(), sub, nil)
			require.NoError(t, err)

			msgs := e.mails(t)
			i := slices.IndexFunc(msgs, func(m mail.Message) bool { return m.To == c.email })
			require.GreaterOrEqual(t, i, 0)
			ack := msgs[i].Text
			assert.Contains(t, ack, "Pour suivre ta demande")
			assert.NotContains(t, ack, "\n\n\n", "no stray blank line")
			assert.Equal(t, c.want, strings.Contains(ack, line), ack)
			for _, m := range msgs {
				if m.To != c.email {
					assert.NotContains(t, m.Text, line, "the club mail stays the same")
				}
			}
		})
	}
}
