// Package sosvpdive holds the business content embedded in the binary
// (spec §9.7): YAML configuration files under config/.
package sosvpdive

import "embed"

// Content holds config/*.yaml. Lot 3 adds kb/.
//
//go:embed config
var Content embed.FS
