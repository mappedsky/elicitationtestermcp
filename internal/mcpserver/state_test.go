package mcpserver

import (
	"strings"
	"testing"
	"time"
)

func TestSignedStateRoundTrips(t *testing.T) {
	sign, err := newSigner("secret", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	token, err := sign.sign(state{Scenario: "form/minimal", Tool: "run_scenario", Round: 1, ArgsDigest: "abc"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := sign.verify(token, "run_scenario", "abc")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Scenario != "form/minimal" || got.Round != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestStateIsRejectedOutsideTheTermsItWasIssuedOn(t *testing.T) {
	sign, err := newSigner("secret", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	token, err := sign.sign(state{Scenario: "form/minimal", Tool: "run_scenario", ArgsDigest: "abc"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	other, err := newSigner("different", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	for name, check := range map[string]func() error{
		"another server's key": func() error { _, err := other.verify(token, "run_scenario", "abc"); return err },
		"another tool":         func() error { _, err := sign.verify(token, "custom_form", "abc"); return err },
		"other arguments":      func() error { _, err := sign.verify(token, "run_scenario", "zzz"); return err },
		"no separator":         func() error { _, err := sign.verify("nodot", "run_scenario", "abc"); return err },
		"edited payload": func() error {
			_, err := sign.verify("x"+token, "run_scenario", "abc")
			return err
		},
		"edited signature": func() error {
			_, err := sign.verify(editSignature(token), "run_scenario", "abc")
			return err
		},
	} {
		if err := check(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestStateExpires(t *testing.T) {
	sign, err := newSigner("secret", time.Nanosecond)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	token, err := sign.sign(state{Scenario: "form/minimal", Tool: "run_scenario", ArgsDigest: "abc"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := sign.verify(token, "run_scenario", "abc"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("got %v, want an expiry error", err)
	}
}

func TestRandomKeysDoNotVerifyEachOther(t *testing.T) {
	first, err := newSigner("", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	second, err := newSigner("", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	token, err := first.sign(state{Scenario: "form/minimal", Tool: "run_scenario", ArgsDigest: "abc"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := second.verify(token, "run_scenario", "abc"); err == nil {
		t.Error("a state signed by one process verified in another; the default key is not random")
	}
}

// editSignature returns token carrying a signature that really is a different
// 32 bytes. It edits the first base64 character rather than the last: 32 bytes
// do not fill 43 base64 characters, so the final character has spare low bits
// that the decoder ignores, and several distinct characters there decode to
// the identical signature. Every state is signed over a fresh nonce, so a last
// character that happened to share its value's encoding class edited nothing
// and the tampered token verified.
func editSignature(token string) string {
	body, signature, _ := strings.Cut(token, ".")
	swapped := "A"
	if strings.HasPrefix(signature, swapped) {
		swapped = "B"
	}
	return body + "." + swapped + signature[1:]
}
