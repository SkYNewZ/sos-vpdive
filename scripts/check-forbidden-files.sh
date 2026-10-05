#!/bin/sh
# Fails when private material is tracked: spreadsheets, CSV, databases, env
# files. Synthetic workbooks directly under testdata/fixtures/ are allowed.
set -eu
bad=$(git ls-files |
	grep -Ei '\.(xlsx|xls|xlsm|csv|db|sqlite|sqlite3)$|\.db-(wal|shm|journal)$|(^|/)\.env($|\.)' |
	grep -Ev '^testdata/fixtures/[^/]+\.xlsx$|(^|/)\.env\.example$' || true)
if [ -n "$bad" ]; then
	echo "Forbidden files are tracked:" >&2
	echo "$bad" >&2
	exit 1
fi
