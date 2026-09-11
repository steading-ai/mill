package cache_test

import (
	"encoding/json"
	"os"
)

// writeJSON and readJSON are the spec's own, so a spec can put a manifest
// or a record where the store reads it.
func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644) //nolint:gosec // a spec's own file
}

func readJSON(path string, value any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // a spec's own file
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
