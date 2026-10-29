package system

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func helperVerifier(mode string) *execVerifier {
	exe, _ := os.Executable()

	return &execVerifier{argv: []string{exe}, env: []string{helperEnv + "=" + mode}}
}

func TestExecVerifierMapsExitCodes(t *testing.T) {
	if ok, err := helperVerifier("match").Verify("alice", "long enough password"); !ok || err != nil {
		t.Fatalf("got %v, %v", ok, err)
	}
	if ok, err := helperVerifier("mismatch").Verify("alice", "long enough password"); ok || err != nil {
		t.Fatalf("a mismatch is a normal false, got %v, %v", ok, err)
	}
	ok, err := helperVerifier("fail").Verify("alice", "long enough password")
	if ok || !errors.Is(err, ErrHelperFailed) {
		t.Fatalf("other exit codes are errors, got %v, %v", ok, err)
	}
}

func TestExecVerifierRejectsInvalidInputBeforeRunning(t *testing.T) {
	verifier := &execVerifier{argv: []string{"/definitely/not/there"}}
	for _, name := range []string{"", "../x", "-rf", "Alice", "a b"} {
		if ok, err := verifier.Verify(name, "long enough password"); ok || err != nil {
			t.Fatalf("name %q: got %v, %v", name, ok, err)
		}
	}
	for _, password := range []string{"", "short", "has\nnewline in it", strings.Repeat("a", 200)} {
		if ok, err := verifier.Verify("alice", password); ok || err != nil {
			t.Fatalf("password %q: got %v, %v", password, ok, err)
		}
	}
}

func TestFakeVerifier(t *testing.T) {
	verifier := &FakeVerifier{Passwords: map[string]string{"amy": "secret password"}}
	if ok, _ := verifier.Verify("amy", "secret password"); !ok {
		t.Fatal("right password must pass")
	}
	if ok, _ := verifier.Verify("amy", "other"); ok {
		t.Fatal("wrong password must fail")
	}
	if ok, _ := verifier.Verify("nobody", "secret password"); ok {
		t.Fatal("unknown user must fail")
	}
}
