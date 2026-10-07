package calendar

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Spec §4.5 as amended: an erasure reaches the participations of the name
// hash, the unregistered ones whose full name is the member's in either
// order, and every other participation of these people, a homonym's
// included.
func TestEraseReachesEveryParticipationOfThePerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00",
			registered(101, "Martin", "Léa"), registered(202, "Bernard", "Hugo"), unregistered(303, "Paul Garnier")),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00",
			unregistered(101, "J.-L. MARTIN"), unregistered(404, "Lea MARTIN"), unregistered(505, "Leroy")),
		event(t, "evt-c", "2026-12-06T08:00:00+01:00", registered(606, "MARTIN", "Lea")),
	)))
	lea := f.keys.Hash(secure.NameKey("Martin", "Léa"))

	n, err := f.store.Count(ctx, lea, "Martin", "Léa")
	require.NoError(t, err)
	assert.Equal(t, 4, n, "101 twice, 404 by full name, 606 the homonym")

	none, err := f.store.Count(ctx, f.keys.Hash(secure.NameKey("Leroy", "")), "Leroy", "")
	require.NoError(t, err)
	assert.Zero(t, none, "an empty first name reaches no unregistered « Leroy »")
	absent, err := f.store.Count(ctx, nil, "", "")
	require.NoError(t, err)
	assert.Zero(t, absent, "a person outside the members list reaches nothing")

	var erased int
	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		erased, err = f.store.EraseTx(ctx, tx, lea, "Martin", "Léa")
		return err
	}))
	assert.Equal(t, 4, erased)
	assert.Equal(t, []Participant{registered(202, "Bernard", "Hugo"), unregistered(303, "Paul Garnier")}, f.people(t, "evt-a"))
	assert.Equal(t, []Participant{unregistered(505, "Leroy")}, f.people(t, "evt-b"))
	assert.Empty(t, f.people(t, "evt-c"))
	assert.Len(t, f.events(t), 3, "events stay")
}

// The producer sends an unregistered participant's full name: once the
// registration that linked it is gone, the name still reaches it.
func TestEraseOutlivesTheRegistration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.push(t, window(
		event(t, "evt-a", "2026-10-11T08:00:00+02:00", registered(101, "Martin", "Léa")),
		event(t, "evt-b", "2026-11-15T08:00:00+01:00", unregistered(101, "MARTIN Léa"), unregistered(707, "Hugo Bernard")),
	)))
	require.NoError(t, f.push(t, window(
		event(t, "evt-b", "2026-11-15T08:00:00+01:00", unregistered(101, "MARTIN Léa"), unregistered(707, "Hugo Bernard")),
	)), "evt-a deleted in VPDive")

	lea := f.keys.Hash(secure.NameKey("Martin", "Léa"))
	n, err := f.store.Count(ctx, lea, "Martin", "Léa")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NoError(t, store.Tx(ctx, f.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.store.EraseTx(ctx, tx, lea, "Martin", "Léa")
		return err
	}))
	assert.Equal(t, []Participant{unregistered(707, "Hugo Bernard")}, f.people(t, "evt-b"))
}
