package admins

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"

	dicebear "github.com/dicebear/dicebear-go/v10"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// avatarBackground is a light neutral tint behind the drawn figure.
const avatarBackground = "e6ebf0"

// avatarURI renders a DiceBear Voxel Art avatar (CC0), offline, seeded by
// username, so an avatar is stable and needs no external service (spec §4.1).
func avatarURI(style *dicebear.Style, username string) (template.URL, error) {
	a, err := dicebear.NewAvatar(style, map[string]any{
		"seed":            username,
		"backgroundColor": []string{avatarBackground},
	})
	if err != nil {
		return "", fmt.Errorf("avatar: %w", err)
	}
	// The markup comes from a fixed style; the seed only selects shapes and
	// is never rendered as text, so the data URI is safe for an <img src>.
	return template.URL(a.DataURI()), nil //nolint:gosec // generated SVG, see comment above
}

// avatar is the account's photo, or its drawn avatar when it has none or the
// photo cannot be read: a lost file must not keep the accounts from loading.
func (r *Registry) avatar(ctx context.Context, a Account) (template.URL, error) {
	if a.avatarFile != "" {
		sealed, err := r.avatars.ReadFile(a.avatarFile)
		var photo []byte
		if err == nil {
			photo, err = r.keys.Open(sealed)
		}
		if err == nil {
			// Base64 cannot leave the attribute, whatever the bytes.
			return template.URL("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(photo)), nil //nolint:gosec // see comment above
		}
		r.logger.ErrorContext(ctx, "read account photo", "error", err)
	}
	return avatarURI(r.style, a.Username)
}

// SetAvatar puts photo, a JPEG from images.Avatar, in place of the account's
// avatar; nil goes back to the drawn one. The file is sealed (spec §8.4); the
// previous one is deleted once the change is committed.
//
// ponytail: a crash between the commit and the deletion leaves an orphan file
// of a few KB; sweep the files no account names at startup if they pile up.
func (r *Registry) SetAvatar(ctx context.Context, username string, photo []byte) error {
	var file sql.NullString
	if photo != nil {
		name, err := secure.NewToken()
		if err != nil {
			return err
		}
		if err := r.avatars.WriteFile(name, r.keys.Seal(photo), 0o600); err != nil {
			return fmt.Errorf("write account photo: %w", err)
		}
		file = sql.NullString{String: name, Valid: true}
	}
	var old sql.NullString
	err := store.Tx(ctx, r.db, "accounts.set_avatar", func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT avatar_file FROM accounts WHERE username = ?`, username).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET avatar_file = ? WHERE username = ?`, file, username)
		return err
	})
	if err != nil {
		r.removeAvatar(ctx, file.String)
		return fmt.Errorf("accounts.set_avatar: %w", err)
	}
	r.removeAvatar(ctx, old.String)
	r.reloadAfterWrite(ctx)
	return nil
}

// removeAvatar deletes a photo file; "" and a missing file are no-ops.
func (r *Registry) removeAvatar(ctx context.Context, name string) {
	if name == "" {
		return
	}
	if err := r.avatars.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logger.ErrorContext(ctx, "delete account photo", "error", err)
	}
}
