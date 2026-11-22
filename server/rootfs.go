package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// createExcl creates name in root for writing. O_EXCL means an existing entry of any kind, a link
// included, is never opened: a link planted at the name cannot redirect the write.
func createExcl(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o644)
}

// createUnique creates <stem>-<16 random hex digits><ext> in root. The name cannot be guessed, and
// a name that exists is never reused.
func createUnique(root *os.Root, stem, ext string) (*os.File, string, error) {
	for range 8 {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, "", fmt.Errorf("random name: %w", err)
		}
		name := stem + "-" + hex.EncodeToString(raw[:]) + ext
		f, err := createExcl(root, name)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("no unused file name")
}
