package admins

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHash is computed once: argon2id with 64 MiB is slow on purpose.
var testHash = sync.OnceValue(func() string {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		return "unreachable"
	}
	return h
})

func accountsYAML(accounts ...Account) string {
	var b strings.Builder
	b.WriteString("admins:\n")
	for _, a := range accounts {
		fmt.Fprintf(&b, "  - username: %s\n    name: %s\n    role: %s\n    password_hash: %q\n", a.Username, a.Name, a.Role, a.PasswordHash)
	}
	return b.String()
}

func alice() Account {
	return Account{Username: "alice", Name: "Alice", Role: "Présidente", PasswordHash: testHash()}
}

func bob() Account {
	return Account{Username: "bob", Name: "Bob", Role: "Trésorier", PasswordHash: testHash()}
}

func TestParseValidFile(t *testing.T) {
	accounts, err := Parse([]byte(accountsYAML(alice(), bob())))
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	assert.Equal(t, "Présidente", accounts[0].Role)
	assert.True(t, strings.HasPrefix(string(accounts[0].Avatar), "data:image/svg+xml"))
	assert.NotEqual(t, accounts[0].Avatar, accounts[1].Avatar)

	again, err := Parse([]byte(accountsYAML(alice())))
	require.NoError(t, err)
	assert.Equal(t, accounts[0].Avatar, again[0].Avatar, "avatar is stable for a username")
	assert.Len(t, accounts[0].CredentialHash(), 32)
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	dup := alice()
	badName := alice()
	badName.Username = "Alice Martin"
	noRole := alice()
	noRole.Role = " "
	badHash := alice()
	badHash.PasswordHash = "secret"
	cases := map[string]string{
		"empty file":    "",
		"no accounts":   "admins: []\n",
		"unknown field": accountsYAML(alice()) + "    email: alice@example.org\n",
		"duplicate":     accountsYAML(alice(), dup),
		"bad username":  accountsYAML(badName),
		"empty role":    accountsYAML(noRole),
		"bad hash":      accountsYAML(badHash),
		"half-written":  "admins:\n  - username: al",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(content))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testHash(), "never echo a password hash")
		})
	}
}

func TestParseYAMLErrorEchoesNoValue(t *testing.T) {
	_, err := Parse([]byte("admins: [Zebulon]\n"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "Zebulon")
	assert.Contains(t, err.Error(), "line 1")
}
