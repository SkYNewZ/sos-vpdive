package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// staleMails runs the daily check and returns the staleness mails sent so
// far to the club.
func (e *testEnv) staleMails(t *testing.T) []mail.Message {
	t.Helper()
	require.NoError(t, e.srv.AlertStaleImports(context.Background()))
	var out []mail.Message
	for _, m := range e.mails(t) {
		if strings.HasPrefix(m.Subject, "Import ancien") {
			out = append(out, m)
		}
	}
	return out
}

// Spec §7.6, §13: an import past its maximum age alerts the committee once;
// a new import re-arms the alert.
func TestStaleImportAlertsOnce(t *testing.T) {
	e := newTestEnv(t)
	assert.Empty(t, e.staleMails(t), "nothing imported, nothing ages")

	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)
	e.clock.advance(7*24*time.Hour + time.Hour)
	mails := e.staleMails(t)
	require.Len(t, mails, 1, "payments age after 168 h, members after 336 h")
	assert.Equal(t, clubEmail, mails[0].To)
	assert.Equal(t, "Import ancien : export des paiements", mails[0].Subject)
	assert.Contains(t, mails[0].Text, "02/09/2026")
	assert.Contains(t, mails[0].Text, "script")
	assert.Contains(t, mails[0].Text, "https://"+adminHost+"/imports")
	assert.Len(t, e.staleMails(t), 1, "once per ageing import")

	e.clock.advance(7 * 24 * time.Hour)
	mails = e.staleMails(t)
	require.Len(t, mails, 2)
	assert.Equal(t, "Import ancien : liste des membres", mails[1].Subject)

	e.importPayments(t)
	assert.Len(t, e.staleMails(t), 2, "a fresh import does not age")
	e.clock.advance(8 * 24 * time.Hour)
	mails = e.staleMails(t)
	require.Len(t, mails, 3, "the new import ages in turn")
	assert.Equal(t, "Import ancien : export des paiements", mails[2].Subject)
}

func TestImportMailLabels(t *testing.T) {
	assert.Equal(t, "Import automatique refusé (club)", mail.EventImportRefused.Label())
	assert.Equal(t, "Import ancien (club)", mail.EventImportStale.Label())
}
