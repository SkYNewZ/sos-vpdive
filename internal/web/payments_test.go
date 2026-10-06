package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Spec §7.3 and §13: the « Paiements VPDive » block of a request page, in
// each of its cases, and never on the member's tracking page.
func TestRequestPagePaymentsBlock(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	lea := e.submitTicket(t, "lea.martin@example.org")
	noe := e.submitTicket(t, "noe.durand@example.org")

	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "Aucun paiement importé.")

	e.importPayments(t)
	page := e.openTicket(t, cookie, hugo.ID).body
	for _, want := range []string{
		"Paiements VPDive",
		"Export du fichier créé le 01/09/2026 (indicatif), importé le 02/09/2026 par Alice",
		"Ces données datent de l'import : vérifie dans VPDive avant d'agir.",
		"Ligne créée le", "180,00\u00a0€", "05/01/2026", "60,00\u00a0€", "02/02/2026",
		"Formation RIFAP", "10,50\u00a0€",
		"le carnet sera recrédité à la suppression de la sortie",
		"payé en argent réel : remboursement par le trésorier",
		"remboursement probable",
	} {
		assert.Contains(t, page, want)
	}

	assert.Contains(t, e.openTicket(t, cookie, lea.ID).body, "Plusieurs membres portent ce nom : aucune ligne n'est affichée.")
	e.importMembers(t, "members_minimal.xlsx")
	assert.Contains(t, e.openTicket(t, cookie, noe.ID).body,
		"Le demandeur n'est pas dans la liste des membres : ses paiements ne peuvent pas être rattachés.")

	_, tracking := e.tracking(t, hugo.Token)
	assert.NotContains(t, tracking, "Paiements VPDive")
	assert.NotContains(t, tracking, "180,00")
}
