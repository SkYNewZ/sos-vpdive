package web

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
)

func TestCalendarLabels(t *testing.T) {
	l, err := loadCalendarLabels(sosvpdive.Content)
	require.NoError(t, err)
	assert.Equal(t, categoryLabel{Label: "Plongée loisir", Tint: 1}, l.category("diving leisure"))
	assert.Equal(t, categoryLabel{Label: "Sans catégorie"}, l.category(""))
	assert.Equal(t, categoryLabel{Label: "Sec du Rocher"}, l.category("Sec du Rocher"), "unknown: as received")
	assert.Equal(t, "Mer", label(l.Environments, "natural sea"))
	assert.Equal(t, "Grotte du Cap", label(l.Environments, "Grotte du Cap"))
	assert.Equal(t, "Pilote (Bateau A, proposé), DP", l.roles([]calendar.Role{
		{Name: "Pilote", Boat: "Bateau A"}, {Name: "Directeur de plongée", Confirmed: true},
	}))
	assert.Equal(t, []roleGroup{
		{Label: "DP", Names: "MARTIN Léa"},
		{Label: "Pilote", Names: "BERNARD Hugo (Bateau A, proposé), GARNIER Paul (Bateau B)"},
	}, l.staff([]calendar.Participant{
		{Name: "MARTIN Léa", Roles: []calendar.Role{{Name: "Directeur de plongée", Confirmed: true}}},
		{Name: "BERNARD Hugo", Roles: []calendar.Role{{Name: "Pilote", Boat: "Bateau A"}}},
		{Name: "Sans rôle"},
		{Name: "GARNIER Paul", Roles: []calendar.Role{{Name: "Pilote", Boat: "Bateau B", Confirmed: true}}},
	}))

	for name, body := range map[string]string{
		"tint too high": "categories:\n  x: {label: X, tint: 6}\n",
		"no label":      "categories:\n  x: {tint: 1}\n",
		"empty role":    "roles:\n  Pilote: \" \"\n",
		"unknown field": "colours: {}\n",
	} {
		_, err := loadCalendarLabels(fstest.MapFS{"config/calendar.yaml": {Data: []byte(body)}})
		assert.Error(t, err, name)
	}
}
