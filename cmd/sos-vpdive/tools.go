package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

const minPasswordLength = 12

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

// hashPassword prints the argon2id hash of a password for admins.yaml.
func hashPassword(stdin io.Reader, stdout io.Writer) error {
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(password) < minPasswordLength {
		return fmt.Errorf("password must have at least %d characters", minPasswordLength)
	}
	hash, err := admins.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, hash)
	return err
}

// readPassword reads twice without echo from a terminal; otherwise it reads
// the first line of stdin (for `docker run -i ... hash-password < file`).
func readPassword(stdin io.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		first, err := prompt(f, "Password: ")
		if err != nil {
			return "", err
		}
		second, err := prompt(f, "Again: ")
		if err != nil {
			return "", err
		}
		if first != second {
			return "", errors.New("the two passwords differ")
		}
		return first, nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func prompt(f *os.File, label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
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
