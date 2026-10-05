package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectAndNullIfZero(t *testing.T) {
	db, _ := openTemp(t)
	rows, err := db.QueryContext(t.Context(), `SELECT value FROM json_each('[3, 1, 2]')`)
	got, err := Collect(rows, err, func(r *sql.Rows) (v int, err error) { return v, r.Scan(&v) })
	require.NoError(t, err)
	assert.Equal(t, []int{3, 1, 2}, got)

	rows, err = db.QueryContext(t.Context(), `SELECT 1`)
	_, err = Collect(rows, err, func(*sql.Rows) (int, error) { return 0, assert.AnError })
	require.ErrorIs(t, err, assert.AnError)

	assert.Nil(t, NullIfZero(0))
	assert.Nil(t, NullIfZero(""))
	assert.Equal(t, int64(4), NullIfZero(int64(4)))
}
