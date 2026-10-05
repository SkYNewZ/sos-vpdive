package admins

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

// Registry holds the current accounts and reloads the file when it changes
// (spec §4.1). An invalid file at startup prevents starting; an invalid file
// later is ignored and the previous accounts stay active.
type Registry struct {
	path   string
	logger *slog.Logger

	mu       sync.RWMutex
	accounts map[string]Account
	sum      [sha256.Size]byte
	err      error
}

// Load reads and validates the accounts file.
func Load(path string, logger *slog.Logger) (*Registry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is operator configuration (ADMINS_FILE), not user input
	if err != nil {
		return nil, fmt.Errorf("read accounts file: %w", err)
	}
	accounts, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r := &Registry{path: path, logger: logger, sum: sha256.Sum256(data)}
	r.accounts = byUsername(accounts)
	return r, nil
}

// Get returns the account of username.
func (r *Registry) Get(username string) (Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.accounts[username]
	return a, ok
}

// Err returns why the file in place was refused, or nil when it is in use.
func (r *Registry) Err() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

// Reload re-reads the file. When it changed and is valid, its accounts
// replace the previous ones, and Reload returns the usernames that were
// removed or whose password changed: their sessions must be revoked.
func (r *Registry) Reload() ([]string, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil, r.refuse(fmt.Errorf("read accounts file: %w", err))
	}
	sum := sha256.Sum256(data)
	r.mu.Lock()
	if sum == r.sum {
		r.err = nil
		r.mu.Unlock()
		return nil, nil
	}
	r.mu.Unlock()

	accounts, err := Parse(data)
	if err != nil {
		return nil, r.refuse(err)
	}
	next := byUsername(accounts)

	r.mu.Lock()
	defer r.mu.Unlock()
	var changed []string
	for name, old := range r.accounts {
		if a, ok := next[name]; !ok || a.PasswordHash != old.PasswordHash {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	r.accounts, r.sum, r.err = next, sum, nil
	return changed, nil
}

// Watch reloads the file every interval until ctx ends. It calls onChange
// with the usernames whose sessions must be revoked, and logs a refused file
// once per distinct error.
func (r *Registry) Watch(ctx context.Context, interval time.Duration, onChange func(context.Context, []string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		changed, err := r.Reload()
		if err != nil {
			if err.Error() != lastErr {
				r.logger.ErrorContext(ctx, "accounts file refused, previous accounts kept", "error", err)
				lastErr = err.Error()
			}
			continue
		}
		lastErr = ""
		if len(changed) > 0 {
			r.logger.InfoContext(ctx, "accounts file reloaded", "revoked_accounts", len(changed))
			onChange(ctx, changed)
		}
	}
}

func (r *Registry) refuse(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
	return err
}

func byUsername(accounts []Account) map[string]Account {
	m := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		m[a.Username] = a
	}
	return m
}
