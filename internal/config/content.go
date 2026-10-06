package config

import (
	"bytes"
	"fmt"
	"io/fs"

	"go.yaml.in/yaml/v3"
)

// DecodeYAML is the strict reader of the embedded content files (spec §9.7):
// it decodes name from content into v and refuses unknown keys.
func DecodeYAML(content fs.FS, name string, v any) error {
	data, err := fs.ReadFile(content, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}
