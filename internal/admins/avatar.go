package admins

import (
	"fmt"
	"html/template"

	dicebear "github.com/dicebear/dicebear-go/v10"
)

// Avatar colors follow spec §12.2: the club navy on a light neutral tint.
const (
	avatarColor      = "0b2e4a"
	avatarBackground = "e6ebf0"
	avatarRadius     = 8
)

// avatarURI renders a DiceBear identicon (CC0), offline, seeded by username,
// so an avatar is stable and needs no external service (spec §4.1).
func avatarURI(style *dicebear.Style, username string) (template.URL, error) {
	a, err := dicebear.NewAvatar(style, map[string]any{
		"seed":            username,
		"rowColor":        []string{avatarColor},
		"backgroundColor": []string{avatarBackground},
		"borderRadius":    avatarRadius,
	})
	if err != nil {
		return "", fmt.Errorf("avatar: %w", err)
	}
	// The markup comes from a fixed style; the seed only selects shapes and
	// is never rendered as text, so the data URI is safe for an <img src>.
	return template.URL(a.DataURI()), nil //nolint:gosec // generated SVG, see comment above
}
