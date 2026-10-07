package admins

import (
	"cmp"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	dicebear "github.com/dicebear/dicebear-go/v10"
	"github.com/dicebear/styles/v10"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Errors of the account changes, for the committee pages to explain.
var (
	ErrInvalid         = errors.New("invalid account")
	ErrTaken           = errors.New("username already taken")
	ErrNotFound        = errors.New("no such account")
	ErrTooShort        = fmt.Errorf("password shorter than %d characters", MinPasswordLength)
	ErrSamePassword    = errors.New("new password is the current one")
	ErrPushoverKey     = errors.New("not a Pushover user key")
	ErrPasswordChanged = errors.New("password changed meanwhile")
)

// Registry keeps the accounts in memory, read from the database at startup,
// after each change made through it, and every Watch interval for changes
// made by another process (the reset-password command).
type Registry struct {
	db     *sql.DB
	keys   *secure.Keys
	logger *slog.Logger
	now    func() time.Time
	style  *dicebear.Style

	// OnChange, when set, runs after an account was removed or its password
	// changed: sessions must be revoked (spec §4.1).
	OnChange func(ctx context.Context)

	syncMu  sync.Mutex // one Reload at a time: a slower one never undoes a newer one
	avatars map[string]template.URL

	mu       sync.RWMutex
	accounts map[string]Account
}

// Open loads the accounts of db.
func Open(ctx context.Context, db *sql.DB, keys *secure.Keys, logger *slog.Logger, now func() time.Time) (*Registry, error) {
	style, err := dicebear.NewStyle([]byte(styles.VoxelArt))
	if err != nil {
		return nil, fmt.Errorf("avatar style: %w", err)
	}
	r := &Registry{db: db, keys: keys, logger: logger, now: now, style: style, avatars: map[string]template.URL{}}
	if _, err := r.Reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Get returns the account of username.
func (r *Registry) Get(username string) (Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.accounts[username]
	return a, ok
}

// Has reports whether an account of that username exists.
func (r *Registry) Has(username string) bool {
	_, ok := r.Get(username)
	return ok
}

// Current returns the account behind a session while the session is still
// valid: the account exists and its password has not changed since the
// session began (credentialHash is Account.CredentialHash at login).
func (r *Registry) Current(username string, credentialHash []byte) (Account, bool) {
	a, ok := r.Get(username)
	if !ok || subtle.ConstantTimeCompare(credentialHash, a.CredentialHash()) != 1 {
		return Account{}, false
	}
	return a, true
}

// Accounts returns the accounts sorted by display name, for the committee's
// reassignment and filter lists.
func (r *Registry) Accounts() []Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Account) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Reload reads the accounts again. It returns the usernames that were
// removed or whose password changed since the previous read.
func (r *Registry) Reload(ctx context.Context) ([]string, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	rows, err := r.db.QueryContext(ctx,
		`SELECT username, name, role, password_hash, must_change_password, pushover_user_key FROM accounts`)
	list, err := store.Collect(rows, err, r.scan)
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	next := make(map[string]Account, len(list))
	for _, a := range list {
		next[a.Username] = a
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var changed []string
	for name, old := range r.accounts {
		if a, ok := next[name]; !ok || a.PasswordHash != old.PasswordHash {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	r.accounts = next
	return changed, nil
}

// Watch reloads the accounts every interval until ctx ends, for changes made
// by another process.
func (r *Registry) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var failure string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		failure = r.poll(ctx, failure)
	}
}

// Insert stores a new account as given, password hash included.
func (r *Registry) Insert(ctx context.Context, a Account) error {
	a.Name, a.Role = strings.TrimSpace(a.Name), strings.TrimSpace(a.Role)
	if !ValidUsername(a.Username) || a.Name == "" || a.Role == "" {
		return ErrInvalid
	}
	var key any
	if a.PushoverUserKey != "" {
		key = r.keys.SealString(a.PushoverUserKey)
	}
	return r.update(ctx, "accounts.insert", ErrTaken,
		`INSERT INTO accounts (username, name, role, password_hash, must_change_password, pushover_user_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (username) DO NOTHING`,
		a.Username, r.keys.SealString(a.Name), a.Role, a.PasswordHash, a.MustChangePassword, key, r.now().Unix())
}

// newTemporary draws a temporary password and its hash.
func newTemporary() (password, hash string, err error) {
	password = TemporaryPassword()
	hash, err = HashPassword(password)
	return password, hash, err
}

// Create adds an account with a temporary password, which it returns: it is
// shown once and never stored in clear.
func (r *Registry) Create(ctx context.Context, username, name, role string) (string, error) {
	password, hash, err := newTemporary()
	if err != nil {
		return "", err
	}
	a := Account{Username: username, Name: name, Role: role, PasswordHash: hash, MustChangePassword: true}
	if err := r.Insert(ctx, a); err != nil {
		return "", err
	}
	return password, nil
}

// ResetPassword puts a new temporary password in place, which it returns.
// The account's sessions end.
func (r *Registry) ResetPassword(ctx context.Context, username string) (string, error) {
	password, hash, err := newTemporary()
	if err != nil {
		return "", err
	}
	if err := r.update(ctx, "accounts.reset_password", ErrNotFound,
		`UPDATE accounts SET password_hash = ?, must_change_password = 1 WHERE username = ?`, hash, username); err != nil {
		return "", err
	}
	return password, nil
}

// ChangePassword sets the password the resolver chose and returns its hash,
// for the session that replaces keepSession. The session whose token hash is
// keepSession stays valid; the account's other sessions end.
func (r *Registry) ChangePassword(ctx context.Context, username, password string, keepSession []byte) (string, error) {
	a, ok := r.Get(username)
	if !ok {
		return "", ErrNotFound
	}
	if len([]rune(password)) < MinPasswordLength {
		return "", ErrTooShort
	}
	same, err := VerifyPassword(a.PasswordHash, password)
	if err != nil {
		return "", err
	}
	if same {
		return "", ErrSamePassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	credential := Account{PasswordHash: hash}.CredentialHash()
	err = store.Tx(ctx, r.db, "accounts.change_password", func(ctx context.Context, tx *sql.Tx) error {
		// Only over the password just verified: an owner's reset in between wins.
		res, err := tx.ExecContext(ctx,
			`UPDATE accounts SET password_hash = ?, must_change_password = 0 WHERE username = ? AND password_hash = ?`,
			hash, username, a.PasswordHash)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, ErrPasswordChanged)
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET credential_hash = ? WHERE token_hash = ? AND username = ?`,
			credential, keepSession, username)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("change password: %w", err)
	}
	r.reloadAfterWrite(ctx)
	return hash, nil
}

// SetPushoverKey sets the account's Pushover user key; "" removes it.
func (r *Registry) SetPushoverKey(ctx context.Context, username, key string) error {
	var sealed any
	if key != "" {
		if !ValidPushoverKey(key) {
			return ErrPushoverKey
		}
		sealed = r.keys.SealString(key)
	}
	return r.update(ctx, "accounts.set_pushover_key", ErrNotFound,
		`UPDATE accounts SET pushover_user_key = ? WHERE username = ?`, sealed, username)
}

// Delete removes the account. Its sessions end and its open requests return
// to « à traiter » through OnChange.
func (r *Registry) Delete(ctx context.Context, username string) error {
	return r.update(ctx, "accounts.delete", ErrNotFound, `DELETE FROM accounts WHERE username = ?`, username)
}

// poll is one Watch tick. A failed read is logged once while the same error
// persists: failure is the previous tick's error text, and poll returns this
// tick's, empty on success.
func (r *Registry) poll(ctx context.Context, failure string) string {
	err := r.sync(ctx)
	if err == nil {
		return ""
	}
	if msg := err.Error(); msg != failure {
		r.logger.ErrorContext(ctx, "reload accounts", "error", err)
		return msg
	}
	return failure
}

func (r *Registry) scan(rows *sql.Rows) (Account, error) {
	var (
		a          Account
		name, key  []byte
		mustChange int
	)
	if err := rows.Scan(&a.Username, &name, &a.Role, &a.PasswordHash, &mustChange, &key); err != nil {
		return Account{}, err
	}
	var err error
	if a.Name, err = r.keys.OpenString(name); err != nil {
		return Account{}, fmt.Errorf("account name: %w", err)
	}
	if key != nil {
		if a.PushoverUserKey, err = r.keys.OpenString(key); err != nil {
			return Account{}, fmt.Errorf("account pushover key: %w", err)
		}
	}
	a.MustChangePassword = mustChange == 1
	if a.Avatar, err = r.avatar(a.Username); err != nil {
		return Account{}, err
	}
	return a, nil
}

// avatar is cached: Reload runs every Watch interval. Called with syncMu
// held.
func (r *Registry) avatar(username string) (template.URL, error) {
	if u, ok := r.avatars[username]; ok {
		return u, nil
	}
	u, err := avatarURI(r.style, username)
	if err != nil {
		return "", err
	}
	r.avatars[username] = u
	return u, nil
}

// sync reloads the accounts and runs OnChange when one was removed or its
// password changed. It ignores the caller's cancellation: the change it
// follows is committed, and its sessions must end even if the request that
// made it is gone. It returns the read error: the next Watch tick reads again.
func (r *Registry) sync(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	changed, err := r.Reload(ctx)
	if err != nil {
		return err
	}
	if len(changed) > 0 && r.OnChange != nil {
		r.OnChange(ctx)
	}
	return nil
}

// reloadAfterWrite is sync for the end of a change that is committed: a
// failed read is logged, not returned.
func (r *Registry) reloadAfterWrite(ctx context.Context) {
	if err := r.sync(ctx); err != nil {
		r.logger.ErrorContext(ctx, "reload accounts", "error", err)
	}
}

// update runs one statement on one account, then reloads. A statement that
// changes no row fails with zeroRows.
func (r *Registry) update(ctx context.Context, op string, zeroRows error, query string, args ...any) error {
	err := store.Tx(ctx, r.db, op, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			err = zeroRows
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	r.reloadAfterWrite(ctx)
	return nil
}
