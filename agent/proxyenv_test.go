package agent

import (
	"strings"
	"testing"
)

func envHas(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func envValue(env []string, name string) (string, bool) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

var proxyVarNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"}

func TestToolEnvEmptyIsNil(t *testing.T) {
	if env := toolEnv("", ""); env != nil {
		t.Fatalf("toolEnv(\"\", \"\") = %v, want nil (direct)", env)
	}
}

// AC2, AC43(a, b): with capture off the operator's proxy never reaches a tool, whatever it looks
// like (a socks5 or http proxy, with or without credentials, with a host or an address). Tools
// get the credential-free forwarder in every proxy variable, uniformly.
func TestToolEnvCaptureOffGivesToolsTheForwarderOnly(t *testing.T) {
	for _, global := range []string{
		"socks5://canaryuser:canarypw@proxy.canary.example:1080",
		"http://canaryuser:canarypw@proxy.canary.example:3128",
		"https://canaryuser@proxy.canary.example",
		"http://proxy.canary.example:3128", // unauthenticated: the tool still never learns the address
		"socks5://10.9.8.7:1080",
	} {
		env := toolEnv(global, "")
		for _, name := range proxyVarNames {
			if v, ok := envValue(env, name); !ok || v != "http://127.0.0.1:8789" {
				t.Errorf("%s: %s = %q, want the forwarder", global, name, v)
			}
		}
		if v, _ := envValue(env, "NODE_USE_ENV_PROXY"); v != "1" {
			t.Errorf("%s: NODE_USE_ENV_PROXY = %q", global, v)
		}
		joined := strings.Join(env, "\n")
		for _, leak := range []string{"canary", "10.9.8.7", "1080", "3128", "socks5"} {
			if strings.Contains(joined, leak) {
				t.Errorf("%s: %q is in the tool environment:\n%s", global, leak, joined)
			}
		}
		for _, ca := range []string{"SSL_CERT_FILE=", "CURL_CA_BUNDLE=", "REQUESTS_CA_BUNDLE=", "GIT_SSL_CAINFO=", "NODE_EXTRA_CA_CERTS="} {
			if envHas(env, ca) {
				t.Errorf("%s: capture off must not inject %s", global, ca)
			}
		}
	}
}

func TestToolEnvUsesTheConfiguredForwarderAddress(t *testing.T) {
	SetEgressForwarder("http://127.0.0.1:9911")
	t.Cleanup(func() { SetEgressForwarder("") })
	env := toolEnv("socks5://u:p@10.0.0.1:1080", "")
	for _, name := range proxyVarNames {
		if v, _ := envValue(env, name); v != "http://127.0.0.1:9911" {
			t.Errorf("%s = %q", name, v)
		}
	}
	SetEgressForwarder("")
	if v, _ := envValue(toolEnv("socks5://u:p@10.0.0.1:1080", ""), "HTTP_PROXY"); v != DefaultEgressForwarder {
		t.Errorf("after the reset HTTP_PROXY = %q, want the default %q", v, DefaultEgressForwarder)
	}
}

// Capture on: tools use the recording proxy (no credentials) and trust its CA, as before; the
// forwarder is not involved.
func TestToolEnvCaptureOnKeepsTheRecordingProxyAndCA(t *testing.T) {
	SetEgressForwarder("http://127.0.0.1:9911")
	t.Cleanup(func() { SetEgressForwarder("") })
	env := toolEnv("http://127.0.0.1:8788", "/data/ca.pem")
	for _, name := range proxyVarNames {
		if v, _ := envValue(env, name); v != "http://127.0.0.1:8788" {
			t.Errorf("%s = %q, want the recording proxy", name, v)
		}
	}
	for _, want := range []string{"SSL_CERT_FILE=", "CURL_CA_BUNDLE=", "REQUESTS_CA_BUNDLE=", "GIT_SSL_CAINFO=", "NODE_EXTRA_CA_CERTS="} {
		if !envHas(env, want) {
			t.Errorf("toolEnv with CA missing %s: %v", want, env)
		}
	}
}
