package server

import (
	"os"
	"path/filepath"
	"testing"
)

// AC21: the CA the tools are told to trust (SSL_CERT_FILE and friends) is the copy in the public
// folder, which the tool user can read, not the file in the closed traffic store.
func TestManagerHandsToolsThePublishedCA(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, "127.0.0.1:0", testDSN())
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.mu.Lock()
	m.trafficOn = true
	m.mu.Unlock()
	got := m.ProxyCACert()
	want := filepath.Join(dir, "public", "traffic-ca-cert.pem")
	if got != want {
		t.Fatalf("ProxyCACert %q, want %q", got, want)
	}
	pub, err := os.ReadFile(got)
	if err != nil || len(pub) == 0 {
		t.Fatalf("no published certificate: %v", err)
	}
	store, err := os.ReadFile(filepath.Join(dir, "traffic", "_ca", "mitmproxy-ca-cert.pem"))
	if err != nil || string(store) != string(pub) {
		t.Errorf("the published copy differs from the store's certificate (%v)", err)
	}
	if fi, _ := os.Stat(got); fi.Mode().Perm()&0o004 == 0 {
		t.Errorf("mode %v: the tool user must be able to read it", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "public", "mitmproxy-ca.pem")); err == nil {
		t.Error("the CA key file was published")
	}
}
