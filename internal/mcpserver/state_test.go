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
			_, err := sign.verify(token[:len(token)-1]+"A", "run_scenario", "abc")
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
