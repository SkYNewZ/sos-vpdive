package payments

import (
	"testing"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

// Witness values of the VPayDive columns the tool never reads (spec §7.5):
// none may reach a stored line.
const (
	witnessEnd          = "17/03/2027"
	witnessBilling      = "Facture mensuelle témoin"
	witnessRate         = 0.0123
	witnessCommission   = 1.23
	witnessNet          = 98.77
	witnessAPIStatus    = "Payé témoin"
	witnessTransferAsk  = "02/03/2027"
	witnessTransferDone = "04/03/2027"
	witnessTransfer     = "Terminé témoin"
)

// mollieCreated is the creation date the valid VPayDive fixture declares.
var mollieCreated = time.Date(2026, 9, 1, 18, 24, 0, 0, time.FixedZone("CEST", 2*3600))

// mollieHeader is the header of the VPayDive export (sheet « Transactions
// Mollie »), its 18 columns in order.
var mollieHeader = []any{
	"Nom", "Prénom", "Type Panier", "Prestation", "Date Début", "Date Fin", "Montant Panier", "Payé",
	"Date paiement", "Mode Facturation", "Taux Commission", "Montant Commission", "Montant Net Club",
	"Méthode", "Statut API", "Date Demande Virement", "Date Virement Effectif", "État virement",
}

// mollieRow is one row of the export by column name; the ignored columns
// always hold their witness values.
type mollieRow map[string]any

func (m mollieRow) row() []any {
	out := make([]any, len(mollieHeader))
	for i, h := range mollieHeader {
		out[i] = m[h.(string)]
	}
	out[5], out[9], out[10], out[11], out[12] = witnessEnd, witnessBilling, witnessRate, witnessCommission, witnessNet
	out[14], out[15], out[16], out[17] = witnessAPIStatus, witnessTransferAsk, witnessTransferDone, witnessTransfer
	return out
}

// collected is a line of a Mollie payment, settled in VPDive unless changed.
func collected(last, first, product string, amount any, paidAt any) mollieRow {
	return mollieRow{"Nom": last, "Prénom": first, "Type Panier": product, "Montant Panier": amount,
		"Payé": "Oui", "Date paiement": paidAt, "Méthode": "Carte de crédit"}
}

// hugoOuting is a line of Bernard Hugo paying for an outing.
func hugoOuting(product, service, starts string, amount any, paidAt string) mollieRow {
	m := collected("Bernard", "Hugo", product, amount, paidAt)
	m["Prestation"], m["Date Début"] = service, starts
	return m
}

func mollieSheet(rows ...mollieRow) xlsxtest.Sheet {
	s := make(xlsxtest.Sheet, 0, 1+len(rows))
	s = append(s, mollieHeader)
	for _, r := range rows {
		s = append(s, r.row())
	}
	return s
}

const (
	mollieOuting  = "Sortie Porquerolles"
	mollieCarnet  = "Carte 10 plongées niveau 1 et 2"
	mollieRefunds = "Sortie annulée Levant"
)

// validMollieRows covers the traps of spec §7.5 with invented names, the
// members of members_valid.xlsx: Martin Léa and MARTIN Lea (homonyms), Bernard
// Hugo, Petit Chloé, Durand Noé and Leroy (no first name). Rows, from 2:
//
//	2-4   Bernard Hugo, one payment of three lines at 12/08 14:05
//	5-6   Bernard Hugo, Pay by Bank, both lines « Non » (collected, not settled)
//	7     Bernard Hugo, a negative line alone: a refund
//	8-10  Petit Chloé, a negative line in a positive basket, a line at zero
//	11    Petit Chloé, native dates, a text amount
//	12    Durand Noé, « Non », date with seconds
//	13-14 the homonyms, a minute apart
//	15-16 Inconnu Paul, no member, two lines at one minute
//	17    no name
//	18    Leroy, an unknown « Payé » value
func validMollieRows() []mollieRow {
	byBank := func(product string, amount float64) mollieRow {
		m := collected("Bernard", "Hugo", product, amount, "03/07/2026 18:30")
		m["Payé"], m["Méthode"] = "Non", "Pay by Bank"
		return m
	}
	native := collected("Petit", "Chloé", "Calendrier", "12,50", 46180.5) // 07/06/2026 12:00
	native["Prestation"], native["Date Début"] = "Plongée du bord", 46185 // 12/06/2026
	unsettled := collected("Durand", "Noé", "Baptême", 35.0, "20/07/2026 08:00:00")
	unsettled["Payé"] = "Non"
	unknown := collected("Leroy", "", "Calendrier", 30.0, "01/07/2026 07:30")
	unknown["Payé"] = "En attente"
	return []mollieRow{
		hugoOuting("Calendrier", mollieOuting, "15/08/2026", 40.0, "12/08/2026 14:05"),
		hugoOuting("Location d'un gilet stabilisateur", mollieOuting, "15/08/2026", 8.0, "12/08/2026 14:05"),
		collected("Bernard", "Hugo", "Supplément distance", 5.0, "12/08/2026 14:05"),
		byBank(mollieCarnet, 300),
		byBank("Adhésion", 70),
		hugoOuting("Calendrier", mollieRefunds, "20/07/2026", -40.0, "10/07/2026 09:15"),
		collected("Petit", "Chloé", "Commande vêtements", 50.0, "05/06/2026 20:00"),
		collected("Petit", "Chloé", "Remise vêtements", -10.0, "05/06/2026 20:00"),
		collected("Petit", "Chloé", "Supplément distance", 0, "05/06/2026 20:00"),
		native,
		unsettled,
		collected("MARTIN", "Lea", "Adhésion", 70.0, "02/04/2026 10:00"),
		collected("Martin", "Léa", "Licence fédérale 2026", 40.0, "02/04/2026 10:01"),
		collected("Inconnu", "Paul", "Baptême", 35.0, "15/05/2026 11:11"),
		collected("Inconnu", "Paul", "Location d'une combinaison", 10.0, "15/05/2026 11:11"),
		collected("", "", "Calendrier", 40.0, "16/05/2026 16:00"),
		unknown,
	}
}

func mollieFixtures(tb testing.TB) map[string][]byte {
	tb.Helper()
	missing := mollieSheet(collected("Bernard", "Hugo", "Adhésion", 70.0, "02/04/2026 10:00"))
	header := append([]any{}, mollieHeader...)
	header[7] = "Payé ?"
	missing[0] = header
	return map[string][]byte{
		"vpaydive_valid.xlsx":          xlsxtest.BuildCreated(tb, mollieCreated, mollieSheet(validMollieRows()...)),
		"vpaydive_missing_column.xlsx": xlsxtest.Build(tb, missing),
	}
}
