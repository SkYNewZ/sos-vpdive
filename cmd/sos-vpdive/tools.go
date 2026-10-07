package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// vapidKeys prints a new VAPID key pair in the form .env expects (spec §9.6).
func vapidKeys(stdout io.Writer) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate VAPID key: %w", err)
	}
	private, err := key.Bytes()
	if err != nil {
		return fmt.Errorf("encode VAPID key: %w", err)
	}
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return fmt.Errorf("encode VAPID key: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n# Add VAPID_SUBJECT=mailto:<the club's address>\n",
		base64.RawURLEncoding.EncodeToString(public), base64.RawURLEncoding.EncodeToString(private))
	return err
}

// resetPassword creates an account or puts a temporary password in place,
// and prints that password (spec §4.1 as amended). It runs next to the
// server, without a shell: docker exec -it <container> /sos-vpdive
// reset-password <username>.
func resetPassword(ctx context.Context, getenv func(string) string, args []string, stdout io.Writer) (err error) {
	flags := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "display name of a new account")
	role := flags.String("role", "", "function of a new account")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		return usageError{"reset-password takes -name and -role (new account only), then one username"}
	}
	username := flags.Arg(0)
	cfg, keys, err := loadKeys(getenv)
	if err != nil {
		return err
	}
	if err := ensureDataDir(cfg); err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, store.FileName))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := store.CheckKey(ctx, db, keys); err != nil {
		return err
	}
	registry, err := admins.Open(ctx, db, keys, slog.New(slog.DiscardHandler), time.Now)
	if err != nil {
		return err
	}
	var password string
	if _, exists := registry.Get(username); exists {
		if *name != "" || *role != "" {
			return usageError{"-name and -role only apply to a new account: they never change"}
		}
		password, err = registry.ResetPassword(ctx, username)
	} else {
		if *name == "" || *role == "" {
			return usageError{"no such account: add -name and -role to create it"}
		}
		password, err = registry.Create(ctx, username, *name, *role)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Temporary password for %s: %s\nThe dashes are part of it. The first sign-in asks for a new password.\n", username, password)
	return err
}

// loadKeys reads the configuration and derives the keys from SECRET_KEY.
func loadKeys(getenv func(string) string) (*config.Config, *secure.Keys, error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		return nil, nil, err
	}
	keys, err := secure.NewKeys(cfg.SecretKey)
	if err != nil {
		return nil, nil, err
	}
	return cfg, keys, nil
}

// ensureDataDir creates the data directory, private to the service user.
func ensureDataDir(cfg *config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cfg.DataDir, err)
	}
	return nil
}

// backup copies the database with SQLite's backup API, after checking that
// SECRET_KEY matches it: a backup is useless without its key.
func backup(ctx context.Context, getenv func(string) string, dest string, stdout io.Writer) (err error) {
	cfg, keys, err := loadKeys(getenv)
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.DataDir, store.FileName)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database to back up: %w", err)
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := store.CheckKey(ctx, db, keys); err != nil {
		return err
	}
	if err := store.Backup(ctx, db, dest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "backup written to %s\n", dest)
	return err
}

// restore puts a backup in place of the database. Stop the service first.
func restore(ctx context.Context, getenv func(string) string, src string, stdout io.Writer) error {
	cfg, keys, err := loadKeys(getenv)
	if err != nil {
		return err
	}
	if err := ensureDataDir(cfg); err != nil {
		return err
	}
	dest := filepath.Join(cfg.DataDir, store.FileName)
	if err := store.Restore(ctx, src, dest, keys); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "database restored from %s to %s\n", src, dest)
	return err
}

// healthcheck asks the local server for /healthz with the members host name,
// so host routing needs no exception. The distroless image has no curl.
func healthcheck(ctx context.Context, getenv func(string) string) (err error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	req.Host = cfg.BaseURL.Host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: /healthz answered %d", resp.StatusCode)
	}
	return nil
}
