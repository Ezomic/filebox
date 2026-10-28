package system

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseLogSource(t *testing.T) {
	for _, name := range []string{"sshd", "panel"} {
		if source, ok := ParseLogSource(name); !ok || string(source) != name {
			t.Errorf("%q should be accepted", name)
		}
	}
	for _, name := range []string{"", " sshd", "SSHD", "ssh", "sshd,panel", "../sshd", "-u ssh", "sshd\n", "kernel", "filebox-panel"} {
		if _, ok := ParseLogSource(name); ok {
			t.Errorf("%q must be rejected", name)
		}
	}
}

func TestJournalArgsAreFixedPerSource(t *testing.T) {
	cases := map[LogSource][]string{
		LogSSHD:  {"-u", "ssh", "-n", "7", "--no-pager", "-o", "short-iso", "-r"},
		LogPanel: {"-u", "filebox-panel", "-n", "7", "--no-pager", "-o", "short-iso", "-r"},
	}
	for source, want := range cases {
		if got := journalArgs(source, 7); !slices.Equal(got, want) {
			t.Errorf("%s: got %v", source, got)
		}
	}
}

func TestFilterTail(t *testing.T) {
	lines := []string{"alpha one", "Beta [two]", "gamma (three)", "ALPHA four", "a.b", "axb"}

	if got := filterTail(lines, "", 2); !slices.Equal(got, []string{"a.b", "axb"}) {
		t.Errorf("tail without filter: %v", got)
	}
	if got := filterTail(lines, "alpha", 10); !slices.Equal(got, []string{"alpha one", "ALPHA four"}) {
		t.Errorf("case-insensitive: %v", got)
	}
	if got := filterTail(lines, "[two]", 10); !slices.Equal(got, []string{"Beta [two]"}) {
		t.Errorf("regex characters must be literal: %v", got)
	}
	if got := filterTail(lines, "a.b", 10); !slices.Equal(got, []string{"a.b"}) {
		t.Errorf("dot must be literal: %v", got)
	}
	if got := filterTail(lines, ".*", 10); len(got) != 0 {
		t.Errorf(".* must be literal: %v", got)
	}
	if got := filterTail(lines, "ALPHA", 1); !slices.Equal(got, []string{"ALPHA four"}) {
		t.Errorf("limit keeps the newest: %v", got)
	}
	if got := filterTail(lines, "nomatch", 5); got == nil || len(got) != 0 {
		t.Errorf("no match must be an empty slice: %#v", got)
	}
	long := strings.Repeat("x", maxLogLine+50)
	if got := filterTail([]string{long}, "", 1); len([]rune(got[0])) > maxLogLine+1 {
		t.Errorf("long line not shortened: %d", len(got[0]))
	}
}

func TestFakeLogs(t *testing.T) {
	logs := NewFakeLogs()
	got, err := logs.Read(LogSSHD, 3, "")
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, _ := logs.Read(LogPanel, 500, "listening"); len(got) == 0 {
		t.Fatal("panel sample should contain a listening line")
	}
}

func TestExecLogsReadsNewestFirstAndReverses(t *testing.T) {
	var gotArgs []string
	logs := &execLogs{run: func(_ context.Context, args []string, _ int64) ([]byte, bool, error) {
		gotArgs = args
		return []byte("newest sshd\nmiddle sshd\noldest\n"), false, nil
	}}

	lines, err := logs.Read(LogSSHD, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"middle sshd", "newest sshd"}) {
		t.Fatalf("got %v", lines)
	}
	if !slices.Equal(gotArgs, journalArgs(LogSSHD, 2)) {
		t.Fatalf("args %v", gotArgs)
	}
}

func TestExecLogsWithFilterReadsAWiderWindow(t *testing.T) {
	var gotArgs []string
	logs := &execLogs{run: func(_ context.Context, args []string, _ int64) ([]byte, bool, error) {
		gotArgs = args
		return []byte("c match\nb other\na match\n"), false, nil
	}}

	lines, err := logs.Read(LogPanel, 5, "MATCH")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"a match", "c match"}) {
		t.Fatalf("got %v", lines)
	}
	if !slices.Equal(gotArgs, journalArgs(LogPanel, filterWindow)) {
		t.Fatalf("filter should widen the window, args %v", gotArgs)
	}
}

func TestExecLogsDropsPartialLineWhenTruncated(t *testing.T) {
	logs := &execLogs{run: func(context.Context, []string, int64) ([]byte, bool, error) {
		return []byte("complete 2\ncomplete 1\npartial li"), true, nil
	}}
	lines, err := logs.Read(LogSSHD, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"complete 1", "complete 2"}) {
		t.Fatalf("got %v", lines)
	}
}

func TestExecLogsErrorsHideDetails(t *testing.T) {
	logs := &execLogs{run: func(context.Context, []string, int64) ([]byte, bool, error) {
		return nil, false, fmt.Errorf("secret detail /var/log/journal: permission denied")
	}}
	_, err := logs.Read(LogSSHD, 10, "")
	if !errors.Is(err, ErrLogsUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestExecLogsRejectsBadArguments(t *testing.T) {
	logs := &execLogs{run: func(context.Context, []string, int64) ([]byte, bool, error) {
		t.Fatal("must not run")
		return nil, false, nil
	}}
	if _, err := logs.Read(LogSource("evil"), 10, ""); !errors.Is(err, ErrInvalidLogRequest) {
		t.Fatalf("source: %v", err)
	}
	for _, n := range []int{0, -1, MaxLogLines + 1} {
		if _, err := logs.Read(LogSSHD, n, ""); !errors.Is(err, ErrInvalidLogRequest) {
			t.Fatalf("lines %d: %v", n, err)
		}
	}
}
