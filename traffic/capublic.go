package traffic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	caStoreName  = "mitmproxy-ca-cert.pem"
	caPublicName = "traffic-ca-cert.pem"
	maxCACert    = 1 << 20
)

// PublishCA copies the CA certificate (never the key) from the store to publicDir/traffic-ca-cert.pem,
// so the tool user can read it: the store is closed to that user. CACertPath returns the copy from
// then on. The copy is the server's (mode 0644) and is written to a temporary file under a random
// name that is then renamed over the published name, all through os.Root, so a link planted at the
// published name, at a temporary name or as the folder itself is never written through. A failure
// leaves CACertPath as it was.
func (t *Traffic) PublishCA(publicDir string) error {
	cert, err := t.readCA()
	if err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(publicDir))
	if err != nil {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	defer parent.Close()
	base := filepath.Base(publicDir)
	if err := parent.Mkdir(base, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	lfi, err := parent.Lstat(base)
	if err != nil {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	if !lfi.IsDir() {
		return fmt.Errorf("publish the CA certificate: %s is not a directory (a link or a file)", publicDir)
	}
	pub, err := parent.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	defer pub.Close()
	if pfi, err := pub.Stat("."); err != nil || !os.SameFile(lfi, pfi) {
		return fmt.Errorf("publish the CA certificate: %s changed while it was being opened", publicDir)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	tmp := "." + caPublicName + "." + hex.EncodeToString(raw[:])
	f, err := pub.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o644)
	if err != nil {
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	if _, err := f.Write(cert); err != nil {
		f.Close()
		_ = pub.Remove(tmp)
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		_ = pub.Remove(tmp)
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = pub.Remove(tmp)
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	if err := pub.Rename(tmp, caPublicName); err != nil {
		_ = pub.Remove(tmp)
		return fmt.Errorf("publish the CA certificate: %w", err)
	}
	t.caPublic.Store(filepath.Join(publicDir, caPublicName))
	return nil
}

// readCA reads the certificate from the store without following a link.
func (t *Traffic) readCA() ([]byte, error) {
	root, err := os.OpenRoot(filepath.Join(t.dir, "_ca"))
	if err != nil {
		return nil, fmt.Errorf("publish the CA certificate: %w", err)
	}
	defer root.Close()
	lfi, err := root.Lstat(caStoreName)
	if err != nil {
		return nil, fmt.Errorf("publish the CA certificate: %w", err)
	}
	if !lfi.Mode().IsRegular() {
		return nil, errors.New("publish the CA certificate: the certificate file is not a regular file")
	}
	f, err := root.OpenFile(caStoreName, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("publish the CA certificate: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCACert+1))
	if err != nil {
		return nil, fmt.Errorf("publish the CA certificate: %w", err)
	}
	if len(b) == 0 || len(b) > maxCACert {
		return nil, errors.New("publish the CA certificate: the certificate file is empty or too large")
	}
	return b, nil
}
