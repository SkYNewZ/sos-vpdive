// Package sosvpdive holds the business content embedded in the binary
// (spec §9.7): YAML configuration files under config/ and the knowledge base
// fiches under kb/ (spec §5.1).
package sosvpdive

import "embed"

// Content holds config/*.yaml and kb/*.md.
//
//go:embed config kb
var Content embed.FS
