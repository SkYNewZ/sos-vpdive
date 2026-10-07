package web

import (
	"cmp"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// tints is the number of category tints in css/input.css (.cal-tint-1 to 5).
const tints = 5

// categoryLabel is a calendar category on screen: its label, and its tint
// from 1 to 5, or 0 for the neutral rule.
type categoryLabel struct {
	Label string `yaml:"label"`
	Tint  int    `yaml:"tint"`
}

// calendarLabels translate the raw values of the pushed calendar (spec §9.7,
// lot 8 part 2). A value missing from config/calendar.yaml shows as
// received.
type calendarLabels struct {
	Categories   map[string]categoryLabel `yaml:"categories"`
	Activities   map[string]string        `yaml:"activities"`
	Environments map[string]string        `yaml:"environments"`
	Roles        map[string]string        `yaml:"roles"`
}

func loadCalendarLabels(content fs.FS) (calendarLabels, error) {
	var l calendarLabels
	if err := config.DecodeYAML(content, "config/calendar.yaml", &l); err != nil {
		return calendarLabels{}, err
	}
	for key, c := range l.Categories {
		if strings.TrimSpace(c.Label) == "" || c.Tint < 0 || c.Tint > tints {
			return calendarLabels{}, fmt.Errorf("config/calendar.yaml: categories[%q]: a label and a tint from 0 to %d are required", key, tints)
		}
	}
	for name, m := range map[string]map[string]string{"activities": l.Activities, "environments": l.Environments, "roles": l.Roles} {
		for key, v := range m {
			if strings.TrimSpace(v) == "" {
				return calendarLabels{}, fmt.Errorf("config/calendar.yaml: %s[%q]: empty label", name, key)
			}
		}
	}
	return l, nil
}

// category returns the label and tint of a category key.
func (l calendarLabels) category(key string) categoryLabel {
	if c, ok := l.Categories[key]; ok {
		return c
	}
	return categoryLabel{Label: key}
}

// label returns the label of key in m, or key itself. Labels are validated
// non-empty at load, so an empty value means a missing key.
func label(m map[string]string, key string) string { return cmp.Or(m[key], key) }

// roleNote is what follows a name or a role: its boat and « proposé ».
func roleNote(r calendar.Role) string {
	var parts []string
	if r.Boat != "" {
		parts = append(parts, r.Boat)
	}
	if !r.Confirmed {
		parts = append(parts, "proposé")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// roles writes a participant's roles: « Pilote (Bateau A, proposé), DP ».
func (l calendarLabels) roles(rs []calendar.Role) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = label(l.Roles, r.Name) + roleNote(r)
	}
	return strings.Join(out, ", ")
}

// roleGroup is a role of an event and who holds it, names joined.
type roleGroup struct {
	Label string
	Names string
}

// staff groups an event's roles by label, sorted by label, each name with
// its boat and « proposé ».
func (l calendarLabels) staff(ps []calendar.Participant) []roleGroup {
	names := map[string][]string{}
	for _, p := range ps {
		for _, r := range p.Roles {
			lbl := label(l.Roles, r.Name)
			names[lbl] = append(names[lbl], p.Name+roleNote(r))
		}
	}
	out := make([]roleGroup, 0, len(names))
	for lbl, ns := range names {
		out = append(out, roleGroup{Label: lbl, Names: strings.Join(ns, ", ")})
	}
	slices.SortFunc(out, func(a, b roleGroup) int { return strings.Compare(a.Label, b.Label) })
	return out
}
