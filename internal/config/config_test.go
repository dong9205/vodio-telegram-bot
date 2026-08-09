package config

import "testing"

func TestParseOptionalInt64Env(t *testing.T) {
	t.Setenv("TEST_CHAT_ID", "-5267891219")

	got, err := parseOptionalInt64Env("TEST_CHAT_ID")
	if err != nil {
		t.Fatalf("parseOptionalInt64Env returned error: %v", err)
	}
	if got != -5267891219 {
		t.Fatalf("parseOptionalInt64Env = %d, want %d", got, int64(-5267891219))
	}
}

func TestParseOptionalInt64EnvRejectsInvalidValue(t *testing.T) {
	t.Setenv("TEST_CHAT_ID", "not-a-chat-id")

	if _, err := parseOptionalInt64Env("TEST_CHAT_ID"); err == nil {
		t.Fatal("parseOptionalInt64Env accepted an invalid chat id")
	}
}

func TestParseMTProxyURL(t *testing.T) {
	t.Setenv("MT_PROXY_URL", "socks5://archive-user:secret@proxy.internal:7890")

	got, err := parseMTProxyURL()
	if err != nil {
		t.Fatalf("parseMTProxyURL returned error: %v", err)
	}
	if got != "socks5://archive-user:secret@proxy.internal:7890" {
		t.Fatalf("parseMTProxyURL = %q", got)
	}
}

func TestParseMTProxyURLAllowsSOCKS5H(t *testing.T) {
	t.Setenv("MT_PROXY_URL", "socks5h://proxy.internal")

	if _, err := parseMTProxyURL(); err != nil {
		t.Fatalf("parseMTProxyURL returned error: %v", err)
	}
}

func TestParseMTProxyURLNormalizesScheme(t *testing.T) {
	t.Setenv("MT_PROXY_URL", "SOCKS5://proxy.internal:7890")

	got, err := parseMTProxyURL()
	if err != nil {
		t.Fatalf("parseMTProxyURL returned error: %v", err)
	}
	if got != "socks5://proxy.internal:7890" {
		t.Fatalf("parseMTProxyURL = %q", got)
	}
}

func TestParseMTProxyURLRejectsUnsupportedScheme(t *testing.T) {
	t.Setenv("MT_PROXY_URL", "http://proxy.internal:7890")

	if _, err := parseMTProxyURL(); err == nil {
		t.Fatal("parseMTProxyURL accepted an HTTP proxy")
	}
}

func TestParseMTProxyURLRejectsMissingHost(t *testing.T) {
	t.Setenv("MT_PROXY_URL", "socks5://")

	if _, err := parseMTProxyURL(); err == nil {
		t.Fatal("parseMTProxyURL accepted a missing host")
	}
}
