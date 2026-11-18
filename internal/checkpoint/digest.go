package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// fileDigest returns the size and the hexadecimal SHA-256 of one file.
func fileDigest(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open %s for its checksum: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", fmt.Errorf("read %s for its checksum: %w", path, err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
