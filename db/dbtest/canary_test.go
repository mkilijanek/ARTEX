package dbtest

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeCanaryDetectionNamesTheKindNeverTheText(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://upg:realpassword1@db.invalid/artextest_dbtest")
	for text, kind := range map[string]string{
		"prefix " + RowCanary + " suffix":    "row canary",
		"x" + PasswordCanary:                 "password canary",
		"leaked realpassword1 in a log line": "ARTEX_PG_DSN password",
	} {
		got, found := FindSecret(text)
		if !found || got != kind {
			t.Errorf("FindSecret(%q) = %q, %v; want %q", text, got, found, kind)
		}
	}
	for _, clean := range []string{"", "table assets: 3 -> 3 rows", "SQLSTATE 23505"} {
		if kind, found := FindSecret(clean); found {
			t.Errorf("FindSecret(%q) reported %s", clean, kind)
		}
	}
}

func TestUpgradeRegisteredSecretIsDetectedUntilRemoved(t *testing.T) {
	remove, err := RegisterSecret("test secret", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if kind, found := FindSecret("a log line with 0123456789abcdef in it"); !found || kind != "test secret" {
		t.Errorf("FindSecret = %q, %v; want the registered kind", kind, found)
	}
	remove()
	if kind, found := FindSecret("a log line with 0123456789abcdef in it"); found {
		t.Errorf("a removed secret is still reported as %s", kind)
	}
	if _, err := RegisterSecret("short", "abc"); err == nil {
		t.Error("a secret shorter than the minimum was registered")
	}
}

func TestUpgradeShortServerPasswordIsNotMatchedAgainstOrdinaryText(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://upg:pw@db.invalid/artextest_dbtest")
	if _, found := FindSecret("the pw column"); found {
		t.Fatal("a two-letter password matched ordinary text")
	}
}

// recordingTB captures a Fatalf message and stops the goroutine, as a real
// testing.TB does.
type recordingTB struct {
	testing.TB
	msg string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestUpgradeAssertNoSecretFailureDoesNotRepeatTheText(t *testing.T) {
	rec := &recordingTB{TB: t}
	text := "row says " + RowCanary + "-detail"
	done := make(chan struct{})
	go func() {
		defer close(done)
		AssertNoSecret(rec, "a failure message", text)
	}()
	<-done
	if rec.msg == "" {
		t.Fatal("no failure reported")
	}
	if strings.Contains(rec.msg, RowCanary) || strings.Contains(rec.msg, "detail") {
		t.Fatalf("failure message repeats the text: %q", rec.msg)
	}
	if !strings.Contains(rec.msg, "row canary") || !strings.Contains(rec.msg, "a failure message") {
		t.Fatalf("failure message does not name what was checked: %q", rec.msg)
	}
}

func TestUpgradeCaptureLogReturnsWhatWasLogged(t *testing.T) {
	read := CaptureLog(t)
	log.Printf("hello %d", 1)
	if got := read(); got != "hello 1\n" {
		t.Fatalf("captured %q", got)
	}
}

func TestUpgradeSuffixEnv(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5432/artextest?sslmode=disable": "postgres://u:p@h:5432/artextest_x?sslmode=disable",
		"postgres://u@h/artextest":                        "postgres://u@h/artextest_x",
		"postgres://u@h":                                  "postgres://u@h",
		"host=h dbname=artextest":                         "host=h dbname=artextest",
		"":                                                "",
	}
	for in, want := range cases {
		t.Setenv(EnvDSN, in)
		SuffixEnv("x")
		if got := envDSN(); got != want {
			t.Errorf("SuffixEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
