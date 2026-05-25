package internal

import (
	"encoding/json"
	"os"
)

func metaPath(path string) string {
	return path + ".json"
}

func writeMetadata(path string, meta Metadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(path), b, 0o600)
}

func readMetadata(path string) (Metadata, error) {
	b, err := os.ReadFile(metaPath(path))
	if err != nil {
		return Metadata{}, err
	}
	var meta Metadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}
