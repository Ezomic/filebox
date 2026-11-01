package system

import (
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestValidQuotaBytes(t *testing.T) {
	for _, bytes := range []int64{MinQuotaBytes, MinQuotaBytes + 1, 10 << 30, MaxQuotaBytes} {
		if !ValidQuotaBytes(bytes) {
			t.Errorf("%d should be valid", bytes)
		}
	}
	for _, bytes := range []int64{math.MinInt64, -1, 0, 1, MinQuotaBytes - 1, MaxQuotaBytes + 1, math.MaxInt64} {
		if ValidQuotaBytes(bytes) {
			t.Errorf("%d should be invalid", bytes)
		}
	}
}

func TestFakeUsersQuotaLifecycle(t *testing.T) {
	users := NewFakeUsers("amy", "bob")
	users.SetUsed("amy", 5<<20)

	quotas, err := users.Quotas()
	if err != nil || len(quotas) != 2 || quotas["amy"].Used != 5<<20 || quotas["amy"].Limit != 0 {
		t.Fatalf("a new user starts with no limit: %v, %v", quotas, err)
	}
	if err := users.SetQuota("amy", 10<<20); err != nil {
		t.Fatal(err)
	}
	if quotas, _ = users.Quotas(); quotas["amy"].Limit != 10<<20 || quotas["bob"].Limit != 0 {
		t.Fatalf("got %v", quotas)
	}
	if err := users.ClearQuota("amy"); err != nil {
		t.Fatal(err)
	}
	if quotas, _ = users.Quotas(); quotas["amy"].Limit != 0 {
		t.Fatalf("got %v", quotas)
	}
}

func TestFakeUsersQuotaRefusesWhatTheHelperRefuses(t *testing.T) {
	users := NewFakeUsers("amy")

	if err := users.SetQuota("ghost", 10<<20); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := users.ClearQuota("ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetQuota("amy", 1); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("got %v", err)
	}
	users.QuotasOff = true
	if _, err := users.Quotas(); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetQuota("amy", 10<<20); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := users.ClearQuota("amy"); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestFakeUsersDeleteForgetsTheQuota(t *testing.T) {
	users := NewFakeUsers("amy")
	_ = users.SetQuota("amy", 10<<20)
	_ = users.Delete("amy")
	_ = users.Create("amy", "a perfectly fine password")

	if quotas, _ := users.Quotas(); quotas["amy"].Limit != 0 {
		t.Fatalf("a recreated user must not inherit the old limit: %v", quotas)
	}
}

func TestParseQuotaList(t *testing.T) {
	quotas, err := parseQuotaList("amy 1048576 none\nbob 0 2097152\ncleo - -\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(quotas) != 2 || quotas["amy"] != (Quota{Used: 1048576}) || quotas["bob"] != (Quota{Limit: 2097152}) {
		t.Fatalf("got %v", quotas)
	}
	if _, ok := quotas["cleo"]; ok {
		t.Fatal("a folder that is not a subvolume has no quota to show")
	}
}

func TestParseQuotaListRejectsGarbage(t *testing.T) {
	bad := []string{
		"amy 1 2 3\n", "amy x none\n", "amy 1 y\n", "amy -1 none\n", "amy 1\n", "../x 1 none\n",
		"amy 99999999999999999999 none\n", "amy 1 none\namy 2 none\n", "amy 01 none\n", "amy 1 0\n",
	}
	for _, out := range bad {
		if quotas, err := parseQuotaList(out); err == nil {
			t.Errorf("%q should be refused, got %v", out, quotas)
		}
	}
}

func TestExecUsersQuotaCallsTheHelperWithExactArguments(t *testing.T) {
	if err := helperUsers("expect-set").SetQuota("alice", 10<<20); err != nil {
		t.Fatal(err)
	}
	if err := helperUsers("expect-clear").ClearQuota("alice"); err != nil {
		t.Fatal(err)
	}
}

func TestExecUsersQuotaRejectsInvalidInputBeforeRunning(t *testing.T) {
	users := &execUsers{argv: []string{"/definitely/not/there"}}

	if err := users.SetQuota("../x", 10<<20); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetQuota("alice", 0); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetQuota("alice", MaxQuotaBytes+1); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("got %v", err)
	}
	if err := users.ClearQuota("-rf"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("got %v", err)
	}
}

func TestExecUsersQuotasParsesHelperOutput(t *testing.T) {
	quotas, err := helperUsers("quota-list").Quotas()
	if err != nil || quotas["amy"].Used != 4096 || quotas["amy"].Limit != 1<<20 {
		t.Fatalf("got %v, %v", quotas, err)
	}
}

func TestExecUsersMapsTheUnavailableExitCode(t *testing.T) {
	users := helperUsers("unavailable")

	if _, err := users.Quotas(); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := users.SetQuota("alice", 10<<20); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := users.ClearQuota("alice"); !errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := helperUsers("fail").ClearQuota("alice"); !errors.Is(err, ErrHelperFailed) || errors.Is(err, ErrQuotasUnavailable) {
		t.Fatalf("other failures stay failures: %v", err)
	}
}

func TestOverQuotaErrorMapping(t *testing.T) {
	d := &dirFiles{}
	if err := d.classify("x", &os.PathError{Op: "write", Path: "x", Err: syscall.EDQUOT}); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("EDQUOT must become ErrOverQuota: %v", err)
	}
	if err := quotaError(syscall.EDQUOT); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("got %v", err)
	}
	if err := quotaError(errors.New("other")); errors.Is(err, ErrOverQuota) {
		t.Fatal("only EDQUOT is a quota error")
	}
	if quotaError(nil) != nil {
		t.Fatal("nil stays nil")
	}
}

func TestWriteReportsOverQuotaWhenTheDiskRefusesTheCopy(t *testing.T) {
	files, err := NewDirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.(io.Closer).Close() })
	reader := &failingReader{err: &os.PathError{Op: "write", Path: "x", Err: syscall.EDQUOT}}

	// The failing reader stands in for the kernel: a write error reaches the copy as an error from the file, so
	// the mapping has to happen on whatever copyToTemp returns.
	if err := files.Write("big.bin", reader, false); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("got %v", err)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// The helper validates its arguments before it checks for root, so these run without privileges or btrfs.
func TestHelperScriptRejectsBadArguments(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs bash on a unix host")
	}
	script, err := filepath.Abs("../../deploy/filebox-user")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing name", []string{"quota-set"}, "usage"},
		{"missing amount", []string{"quota-set", "alice"}, "usage"},
		{"extra argument", []string{"quota-set", "alice", "10485760", "x"}, "usage"},
		{"clear extra argument", []string{"quota-clear", "alice", "x"}, "usage"},
		{"list extra argument", []string{"quota-list", "alice"}, "usage"},
		{"reserved name", []string{"quota-set", "root", "10485760"}, "reserved"},
		{"slash in name", []string{"quota-set", "../etc", "10485760"}, "invalid name"},
		{"option as name", []string{"quota-clear", "-rf"}, "invalid name"},
		{"empty amount", []string{"quota-set", "alice", ""}, "invalid amount"},
		{"zero", []string{"quota-set", "alice", "0"}, "invalid amount"},
		{"negative", []string{"quota-set", "alice", "-5"}, "invalid amount"},
		{"plus sign", []string{"quota-set", "alice", "+10485760"}, "invalid amount"},
		{"exponent", []string{"quota-set", "alice", "1e9"}, "invalid amount"},
		{"unit suffix", []string{"quota-set", "alice", "10GB"}, "invalid amount"},
		{"decimal", []string{"quota-set", "alice", "10485760.5"}, "invalid amount"},
		{"leading zeros", []string{"quota-set", "alice", "010485760"}, "invalid amount"},
		{"whitespace", []string{"quota-set", "alice", " 10485760"}, "invalid amount"},
		{"trailing newline", []string{"quota-set", "alice", "10485760\n"}, "invalid amount"},
		{"hex", []string{"quota-set", "alice", "0x100000"}, "invalid amount"},
		{"unicode digits", []string{"quota-set", "alice", "١٠٤٨٥٧٦٠"}, "invalid amount"},
		{"command substitution", []string{"quota-set", "alice", "$(id)"}, "invalid amount"},
		{"below minimum", []string{"quota-set", "alice", "1048575"}, "between"},
		{"above maximum", []string{"quota-set", "alice", "1125899906842625"}, "between"},
		{"over 2^63", []string{"quota-set", "alice", "9223372036854775808"}, "invalid amount"},
		{"very long", []string{"quota-set", "alice", strings.Repeat("9", 400)}, "invalid amount"},
		{"convert reserved", []string{"convert", "root"}, "reserved"},
		{"convert-finish bad name", []string{"convert-finish", "A b"}, "invalid name"},
		{"dry-run alone", []string{"--dry-run"}, "usage"},
		{"dry-run on create", []string{"--dry-run", "create", "alice"}, "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(bash, append([]string{script}, tc.args...)...).CombinedOutput()
			if err == nil {
				t.Fatalf("expected a refusal, got %q", out)
			}
			if !strings.Contains(string(out), tc.want) || strings.Contains(string(out), "must run as root") {
				t.Fatalf("got %q, want it to mention %q before the root check", out, tc.want)
			}
		})
	}
}
