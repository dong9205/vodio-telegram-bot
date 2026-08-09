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
