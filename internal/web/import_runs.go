package web

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
)

// The runs journal of the imports page (design 2026-10-09): under the latest
// import of each export, its latest runs and a chart of the last 30 days,
// drawn like the usage dashboard.
const (
	journalDays   = 30
	journalRecent = 5
	markHeight    = 3 // of the failure mark at the foot of a column, which the bar stops above
)

// journal is the latest runs of one export, whatever their age, and its runs
// over the last journalDays.
type journal struct {
	Recent   []runRow // the latest first
	Chart    runsChart
	Runs     int // over journalDays
	Failures int // refused or failed, over journalDays
}

// runRow is a run as the list shows it. The template picks the dot's
// status class from Result itself: Tailwind only scans the templates.
type runRow struct {
	imports.Run

	Outcome string // « Importé : 612 comptes », « Échec : … »
}

// runsChart is a column per Paris day: the rows of the day's last
// successful run, and a mark when a run failed or was refused.
type runsChart struct {
	One, Many string // the unit of its rows
	Heading   string // the unit, capitalised
	Top       string // the value of the top gridline
	Width     int    // of the viewBox
	Columns   []runColumn
}

type runColumn struct {
	axisDay

	X        int // the left edge of its slot
	MarkX    float64
	Rows     int // of the last successful run, 0 without one
	Runs     int
	Failures int
	Bar      string // path data
}

// runUnit names what an export's rows are.
type runUnit struct{ one, many, heading string }

var runUnits = map[imports.Kind]runUnit{
	imports.Members: {"compte", "comptes", "Comptes"}, imports.Payments: {"ligne", "lignes", "Lignes"},
	imports.Mollie: {"ligne", "lignes", "Lignes"}, imports.Calendar: {"événement", "événements", "Événements"},
	imports.Carnets: {"carte", "cartes", "Cartes"},
}

// failureLabels read the classes the script reports; an unknown class shows
// its detail alone.
var failureLabels = map[string]string{
	"sign_in_capped": "connexion à VPDive plafonnée, une tentative par 24 h",
	"vpdive_session": "session VPDive refusée",
	"vpdive_failed":  "lecture de VPDive en échec",
	"push_failed":    "envoi à l'outil en échec",
	"push_refused":   "envoi refusé par l'outil",
}

// buildJournal lists the latest of runs, the latest first, and lays out
// those of the journalDays that end at now in paris.
func buildJournal(kind imports.Kind, runs []imports.Run, now time.Time, paris *time.Location) journal {
	unit := runUnits[kind]
	j := journal{Chart: runsChart{One: unit.one, Many: unit.many, Heading: unit.heading, Width: journalDays * slotWidth}}
	for _, r := range runs[:min(len(runs), journalRecent)] {
		j.Recent = append(j.Recent, runRow{Run: r, Outcome: outcome(r, unit)})
	}
	axis, dayAt := axisDays(midnight(now, paris), journalDays, paris)
	cols := make([]runColumn, journalDays)
	for i, d := range axis {
		cols[i].axisDay = d
		cols[i].X = i * slotWidth
		cols[i].MarkX = float64(cols[i].X + (slotWidth-barWidth)/2)
	}
	for _, r := range slices.Backward(runs) { // oldest first: the day's last success wins
		c, ok := dayAt[parisDay(r.At, paris)]
		if !ok {
			continue
		}
		cols[c].Runs++
		j.Runs++
		switch r.Result {
		case imports.Imported, imports.Unchanged:
			cols[c].Rows = r.Rows
		case imports.Refused, imports.Failed:
			cols[c].Failures++
			j.Failures++
		}
	}
	var largest int64
	for _, c := range cols { // the values drawn, not every success of a day
		largest = max(largest, int64(c.Rows))
	}
	top := roundUp(largest)
	j.Chart.Top = strconv.FormatInt(top, 10)
	for i := range cols {
		h := height(int64(cols[i].Rows), top)
		if h == 0 {
			continue
		}
		bottom := 100.0
		if cols[i].Failures > 0 { // the mark sits under the bar
			bottom -= markHeight + 1
		}
		cols[i].Bar = bar(cols[i].MarkX, min(100-h, bottom-minHeight), bottom, true)
	}
	j.Chart.Columns = cols
	return j
}

// outcome writes a run's result for the committee.
func outcome(r imports.Run, unit runUnit) string {
	switch r.Result {
	case imports.Imported:
		return "Importé : " + plural(r.Rows, unit.one, unit.many)
	case imports.Unchanged:
		return "Fichier identique au précédent"
	case imports.Refused:
		return "Refusé : " + cmp.Or(r.Detail, r.Code)
	case imports.Failed:
	}
	label, known := failureLabels[r.Code]
	switch {
	case known && r.Detail != "":
		return "Échec : " + label + " (" + r.Detail + ")"
	case known:
		return "Échec : " + label
	}
	return "Échec : " + cmp.Or(r.Detail, r.Code)
}
