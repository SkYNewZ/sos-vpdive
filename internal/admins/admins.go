// Package admins loads the committee accounts file (spec §4.1): identifier,
// display name, function and argon2id password hash. Accounts live in this
// file, not in the database, and the file is reloaded when it changes.
package admins

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"

	dicebear "github.com/dicebear/dicebear-go/v10"
	"github.com/dicebear/styles/v10"
	"go.yaml.in/yaml/v3"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

// Account is one committee member. Name and Role are shown wherever the
// account is cited; there are no permission levels.
type Account struct {
	Username     string       `yaml:"username"`
	Name         string       `yaml:"name"`
	Role         string       `yaml:"role"`
	PasswordHash string       `yaml:"password_hash"`
	Avatar       template.URL `yaml:"-"`
}

// CredentialHash fingerprints the password hash. A session stores it and
// stays valid only while it matches the accounts file.
func (a Account) CredentialHash() []byte {
	sum := sha256.Sum256([]byte(a.PasswordHash))
	return sum[:]
}

type accountsFile struct {
	Admins []Account `yaml:"admins"`
}

// Parse decodes and validates an accounts file. Unknown keys are refused.
// Errors cite accounts by position and never echo a value.
func Parse(data []byte) ([]Account, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f accountsFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("accounts file is empty")
		}
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	if len(f.Admins) == 0 {
		return nil, errors.New("accounts file declares no account")
	}
	var errs []error
	first := map[string]int{}
	for i, a := range f.Admins {
		where := fmt.Sprintf("admins[%d]", i)
		if !usernamePattern.MatchString(a.Username) {
			errs = append(errs, fmt.Errorf("%s: username must match %s", where, usernamePattern))
		}
		if j, dup := first[a.Username]; dup {
			errs = append(errs, fmt.Errorf("%s: username duplicates admins[%d]", where, j))
		} else {
			first[a.Username] = i
		}
		if strings.TrimSpace(a.Name) == "" {
			errs = append(errs, fmt.Errorf("%s: name is empty", where))
		}
		if strings.TrimSpace(a.Role) == "" {
			errs = append(errs, fmt.Errorf("%s: role is empty", where))
		}
		if _, err := parseHash(a.PasswordHash); err != nil {
			errs = append(errs, fmt.Errorf("%s: password_hash: %w", where, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	style, err := dicebear.NewStyle([]byte(styles.Identicon))
	if err != nil {
		return nil, fmt.Errorf("avatar style: %w", err)
	}
	for i := range f.Admins {
		if f.Admins[i].Avatar, err = avatarURI(style, f.Admins[i].Username); err != nil {
			return nil, err
		}
	}
	return f.Admins, nil
}
