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
		age                                time.Duration // of the campaign import when the request comes
		want                               bool
	}{
		{"all four hold, something unpaid", "noe.durand@example.org", "Noé", "Durand", "adhesion", everyone, 0, true},
		{"import older than CAMPAIGN_MAX_AGE", "noe.durand@example.org", "Noé", "Durand", "adhesion", everyone, 49 * time.Hour, false},
		{"both paid, to validate: nothing blocks the payment", "hugo.bernard@example.org", "Hugo", "Bernard", "adhesion", everyone, 0, false},
		{"another category", "noe.durand@example.org", "Noé", "Durand", "carnet", everyone, 0, false},
		{"not in the members list, never by the typed name", "nouveau@example.org", "Noé", "Durand", "adhesion", everyone, 0, false},
		{"no campaign line for the member", "noe.durand@example.org", "Noé", "Durand", "adhesion",
			func(p campaign.Person) bool { return p.LastName != "Durand" }, 0, false},
		{"homonyms", "lea.martin@example.org", "Léa", "Martin", "adhesion", everyone, 0, false},
		{"already validated", "chloe.petit@example.org", "Chloé", "Petit", "adhesion", everyone, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestStore(t, func(d *Deps) { d.CampaignMaxAge = 48 * time.Hour })
			memberstest.Import(t, e.members, "members_valid.xlsx")
			e.importCampaign(t, c.keep)
			e.clock.advance(c.age)
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
