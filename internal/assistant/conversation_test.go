package assistant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddPersonOnce(t *testing.T) {
	var c Conversation
	assert.Equal(t, "m1", c.AddPerson(Person{Email: "a@example.org"}))
	assert.Equal(t, "m2", c.AddPerson(Person{Email: "b@example.org"}))
	assert.Equal(t, "m1", c.AddPerson(Person{Email: "a@example.org"}))
	p, ok := c.Person("m2")
	require.True(t, ok)
	assert.Equal(t, "b@example.org", p.Email)
	_, ok = c.Person("m9")
	assert.False(t, ok)

	// Known by the campaign only: no address, one ref per name hash.
	assert.Equal(t, "m3", c.AddPerson(Person{NameHash: []byte("roux")}))
	assert.Equal(t, "m4", c.AddPerson(Person{NameHash: []byte("inconnu")}))
	assert.Equal(t, "m3", c.AddPerson(Person{NameHash: []byte("roux")}))
}
