//go:build unix

package traffic

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func openForCA(t *testing.T) (*Traffic, string) {
	t.Helper()
	data := t.TempDir()
	tr, err := Open(filepath.Join(data, "traffic"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, data
}

// The CA certificate tools must trust is published in the data directory's public folder, where
// the tool user can read it while the rest of the traffic store is closed to it.
func TestPublishCACopiesTheCertificateToThePublicFolder(t *testing.T) {
	tr, data := openForCA(t)
	src := filepath.Join(data, "traffic", "_ca", "mitmproxy-ca-cert.pem")
	want, err := os.ReadFile(src)
	if err != nil || len(want) == 0 {
		t.Fatalf("the proxy wrote no certificate: %v", err)
	}
	if got := tr.CACertPath(); got != src {
		t.Fatalf("before publishing, the path is the store's: %q", got)
	}
	pub := filepath.Join(data, "public")
	if err := tr.PublishCA(pub); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(pub, "traffic-ca-cert.pem")
	if got := tr.CACertPath(); got != dst {
		t.Fatalf("CACertPath %q, want %q", got, dst)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(want) {
		t.Fatalf("copy differs: %v", err)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if pi, _ := os.Stat(pub); pi.Mode().Perm() != 0o755 {
		t.Errorf("public folder mode %v", pi.Mode().Perm())
	}
	if os.Geteuid() == 0 {
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 {
			t.Errorf("owner %d", st.Uid)
		}
	}
	// no temporary file is left behind
	ents, _ := os.ReadDir(pub)
	if len(ents) != 1 {
		t.Errorf("files in the public folder: %v", ents)
	}
	// the private key stays where it was and is not copied
	for _, e := range ents {
		if e.Name() != "traffic-ca-cert.pem" {
			t.Errorf("unexpected file %s", e.Name())
		}
	}
}

// A link a tool planted at the published name, at a guessable temporary name, or as the public
// folder is not written through; the published file is a new regular file.
func TestPublishCANeverWritesThroughALink(t *testing.T) {
	tr, data := openForCA(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(data, "public")
	if err := os.MkdirAll(pub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"traffic-ca-cert.pem", ".traffic-ca-cert.pem.tmp", "traffic-ca-cert.pem.tmp", ".traffic-ca-cert.pem"} {
		if err := os.Symlink(victim, filepath.Join(pub, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.PublishCA(pub); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "ORIGINAL" {
		t.Fatalf("the link target was overwritten: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(pub, "traffic-ca-cert.pem"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the published name is not a regular file: %v %v", fi, err)
	}

	// the folder itself is a link: nothing is written through it and the path stays the store's
	tr2, data2 := openForCA(t)
	if err := os.Symlink(outside, filepath.Join(data2, "public")); err != nil {
		t.Fatal(err)
	}
	if err := tr2.PublishCA(filepath.Join(data2, "public")); err == nil {
		t.Fatal("a linked public folder was accepted")
	}
	if got := tr2.CACertPath(); got != filepath.Join(data2, "traffic", "_ca", "mitmproxy-ca-cert.pem") {
		t.Errorf("after a failed publish the path is %q", got)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Errorf("files appeared outside: %v", ents)
	}
}

// Publishing again (the next start) replaces the copy.
func TestPublishCAReplacesAnOlderCopy(t *testing.T) {
	tr, data := openForCA(t)
	pub := filepath.Join(data, "public")
	if err := os.MkdirAll(pub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pub, "traffic-ca-cert.pem"), []byte("OLD CERT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tr.PublishCA(pub); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(data, "traffic", "_ca", "mitmproxy-ca-cert.pem"))
	if got, _ := os.ReadFile(filepath.Join(pub, "traffic-ca-cert.pem")); string(got) != string(want) {
		t.Errorf("the old copy was kept")
	}
}

func TestPublishCAFailsWithoutACertificate(t *testing.T) {
	tr, data := openForCA(t)
	if err := os.Remove(filepath.Join(data, "traffic", "_ca", "mitmproxy-ca-cert.pem")); err != nil {
		t.Fatal(err)
	}
	if err := tr.PublishCA(filepath.Join(data, "public")); err == nil {
		t.Fatal("published nothing without error")
	}
}
