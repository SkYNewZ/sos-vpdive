#!/bin/sh
# Draws the Mermaid diagram of each fiche in kb/ into kb/<id>.svg, stamped
# with the SHA-256 of its source: kb.Load refuses a drawing that no longer
# matches. Node is a local convenience here, never a build step: the SVGs
# are committed.
set -eu
cd "$(dirname "$0")/.."
cli=@mermaid-js/mermaid-cli@12.0.0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for md in kb/*.md; do
	id=$(basename "$md" .md)
	# The fence lines as kb.blocks sees them: CRs dropped, the closing fence trimmed.
	tr -d '\r' <"$md" | awk '/^[[:space:]]*```mermaid[[:space:]]*$/ {on = 1; next} on && /^[[:space:]]*```[[:space:]]*$/ {exit} on {print}' >"$tmp/$id.mmd"
	[ -s "$tmp/$id.mmd" ] || continue
	npx -y "$cli" -q -i "$tmp/$id.mmd" -o "$tmp/$id.svg" -c scripts/mermaid.json -b transparent
	sum=$(shasum -a 256 "$tmp/$id.mmd" | cut -d ' ' -f 1)
	{
		cat "$tmp/$id.svg"
		printf '\n<!-- mermaid sha256:%s -->\n' "$sum"
	} >"kb/$id.svg"
	echo "kb/$id.svg"
done
