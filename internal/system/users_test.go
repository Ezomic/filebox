package system

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestValidUsername(t *testing.T) {
	valid := []string{"abc", "alice", "a_b", "a-b", "a12", strings.Repeat("a", 32)}
	for _, name := range valid {
		if !ValidUsername(name) {
			t.Errorf("%q should be valid", name)
		}
	}

	invalid := []string{
		"", " ", "   ", "---", "...", "a", "ab", strings.Repeat("a", 33),
		"Alice", "ALICE", "1abc", "-abc", "_abc", " abc", "abc ", "abc\n", "ab\ncd",
		"a/b", "..", "../etc", "a.b", "ab\x00c", "alicé", "аlice", "ａｌｉｃｅ", "a b", "a;b", "a$b", "a'b",
	}
	for _, name := range invalid {
		if ValidUsername(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	ok := []string{
		strings.Repeat("a", 12), strings.Repeat("a", 128), strings.Repeat("é", 12), strings.Repeat("é", 128),
		"quote'\"and$dollar`tick", "has spaces in it ok",
	}
	for _, password := range ok {
		if err := ValidatePassword(password); err != nil {
			t.Errorf("%q should be valid: %v", password, err)
		}
	}

	bad := []string{
		"", strings.Repeat("a", 11), strings.Repeat("é", 11), strings.Repeat("a", 129), strings.Repeat("é", 129),
		"abcdefghijk\x00l", "abcdefghijkl\nm", "abcdefghijkl\rm",
	}
	for _, password := range bad {
		if err := ValidatePassword(password); err == nil {
			t.Errorf("%q should be invalid", password)
		}
	}
}

func TestFakeUsersLifecycle(t *testing.T) {
	users := NewFakeUsers("zed", "amy")

	names, err := users.List()
	if err != nil || !slices.Equal(names, []string{"amy", "zed"}) {
		t.Fatalf("got %v, %v", names, err)
	}
	if err := users.Create("amy", "x"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("got %v", err)
	}
	if err := users.Create("bob", "x"); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPassword("nobody", "x"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetPassword("bob", "new"); err != nil || users.Passwords["bob"] != "new" {
		t.Fatalf("got %v", err)
	}
	if err := users.Delete("bob"); err != nil {
		t.Fatal(err)
	}
	if err := users.Delete("bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestNewUsersFallbackHasSampleUsers(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux uses the real helper")
	}
	names, err := NewUsers().List()
	if err != nil || len(names) == 0 {
		t.Fatalf("got %v, %v", names, err)
	}
}

const helperEnv = "FILEBOX_TEST_HELPER"

// TestMain doubles as the fake filebox-user helper when re-executed by the tests below.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runFakeHelper(mode)
	}
	os.Exit(m.Run())
}

func runFakeHelper(mode string) {
	args := os.Args[1:]
	switch mode {
	case "echo":
		stdin := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			stdin.Write(buf[:n])
			if err != nil {
				break
			}
		}
		fmt.Printf("args=%s\nstdin=%q\n", strings.Join(args, "|"), stdin.String())
		os.Exit(0)
	case "list":
		fmt.Print("amy\n\nbob\n")
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, "useradd: secret internal detail")
		os.Exit(3)
	case "match":
		os.Exit(0)
	case "mismatch":
		os.Exit(10)
	}
	os.Exit(99)
}

func helperUsers(mode string) *execUsers {
	exe, _ := os.Executable()
	t := &execUsers{argv: []string{exe}}
	t.env = []string{helperEnv + "=" + mode}

	return t
}

func TestExecUsersPassesPasswordOnStdinNotArgv(t *testing.T) {
	users := helperUsers("echo")
	out, err := users.run("pa ss'wo\"rd$1", "create", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "args=create|alice\n") {
		t.Fatalf("unexpected args: %q", out)
	}
	if !strings.Contains(out, `stdin="pa ss'wo\"rd$1\n"`) {
		t.Fatalf("password not on stdin: %q", out)
	}
	if strings.Contains(strings.SplitN(out, "\n", 2)[0], "pa ss") {
		t.Fatal("password leaked into argv")
	}
}

func TestExecUsersListSkipsBlankLines(t *testing.T) {
	names, err := helperUsers("list").List()
	if err != nil || !slices.Equal(names, []string{"amy", "bob"}) {
		t.Fatalf("got %v, %v", names, err)
	}
}

func TestExecUsersFailureHidesStderr(t *testing.T) {
	users := helperUsers("fail")
	err := users.Delete("alice")
	if !errors.Is(err, ErrHelperFailed) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "secret internal detail") {
		t.Fatalf("log detail missing: %v", err)
	}
}

func TestExecUsersRejectsInvalidInputBeforeRunning(t *testing.T) {
	users := &execUsers{argv: []string{"/definitely/not/there"}}
	if err := users.Create("../x", "long enough password"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetPassword("alice", "short"); err == nil || errors.Is(err, ErrHelperFailed) {
		t.Fatalf("got %v", err)
	}
	if err := users.Delete("-rf"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("got %v", err)
	}
}
