package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

type logCall struct {
	source system.LogSource
	lines  int
	filter string
}

type spyLogs struct {
	*system.FakeLogs
	calls    []logCall
	failWith error
}

func (s *spyLogs) Read(source system.LogSource, lines int, filter string) ([]string, error) {
	s.calls = append(s.calls, logCall{source, lines, filter})
	if s.failWith != nil {
		return nil, s.failWith
	}

	return s.FakeLogs.Read(source, lines, filter)
}

func logsURL(source, lines, filter string) string {
	query := url.Values{}
	if source != "" {
		query.Set("source", source)
	}
	if lines != "" {
		query.Set("lines", lines)
	}
	if filter != "" {
		query.Set("filter", filter)
	}

	return "/api/logs?" + query.Encode()
}

func TestLogsDefaults(t *testing.T) {
	s := newFileSession(t)
	rec := s.get("/api/logs?source=sshd")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Lines) == 0 {
		t.Fatalf("body %s %v", rec.Body, err)
	}
	if call := s.logs.calls[0]; call.source != system.LogSSHD || call.lines != 100 || call.filter != "" {
		t.Fatalf("defaults wrong: %+v", call)
	}
}

func TestLogsAcceptsEachSourceAndLineBounds(t *testing.T) {
	s := newFileSession(t)
	for _, source := range []string{"sshd", "panel"} {
		for _, lines := range []string{"1", "2", "499", "500"} {
			if rec := s.get(logsURL(source, lines, "")); rec.Code != http.StatusOK {
				t.Errorf("%s lines=%s: %d", source, lines, rec.Code)
			}
		}
	}
	if call := s.logs.calls[len(s.logs.calls)-1]; call.lines != 500 || call.source != system.LogPanel {
		t.Fatalf("last call %+v", call)
	}
}

func TestLogsRejectsBadSource(t *testing.T) {
	s := newFileSession(t)
	sources := []string{"", " ", "SSHD", "ssh", "kernel", "sshd panel", "sshd,panel", "../../etc/passwd", "-u ssh", "sshd;reboot", "sshd\nreboot", "$(id)", "`id`", "|", "'", "filebox-panel", strings.Repeat("a", 5000), "ｓｓｈｄ", "sshd\x00"}
	for _, source := range sources {
		rec := s.call("GET", "/api/logs?source="+url.QueryEscape(source), nil, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("source %q: got %d, want 422", source, rec.Code)
		}
		apiError(t, rec)
	}
	if rec := s.get("/api/logs"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing source: %d", rec.Code)
	}
	if len(s.logs.calls) != 0 {
		t.Fatalf("rejected requests still reached the log reader: %v", s.logs.calls)
	}
}

func TestLogsRejectsBadLineCounts(t *testing.T) {
	s := newFileSession(t)
	for _, lines := range []string{"0", "501", "-1", "abc", " 5", "5 ", "1e2", "5.0", "0x10", "+5", "٥", "99999999999999999999", "-0", "1,2", "%00", ""} {
		target := "/api/logs?source=sshd&lines=" + url.QueryEscape(lines)
		if rec := s.get(target); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("lines %q: got %d, want 422", lines, rec.Code)
		}
	}
	if len(s.logs.calls) != 0 {
		t.Fatalf("rejected requests still reached the log reader: %v", s.logs.calls)
	}
}

func TestLogsFilterIsPassedThroughLiterally(t *testing.T) {
	s := newFileSession(t)
	filters := []string{"Failed password", "[a-z]+", ".*", "(", "\\", "$(id)", "a|b", "日本語", "ünï", "!!!", "  padded  ", strings.Repeat("a", 100), strings.Repeat("日", 100)}
	for _, filter := range filters {
		s.logs.calls = nil
		rec := s.get(logsURL("panel", "50", filter))
		if rec.Code != http.StatusOK {
			t.Errorf("filter %q: got %d", filter, rec.Code)
			continue
		}
		if len(s.logs.calls) != 1 || s.logs.calls[0].filter != filter || s.logs.calls[0].lines != 50 {
			t.Errorf("filter %q arrived as %+v", filter, s.logs.calls)
		}
	}
}

func TestLogsRejectsBadFilters(t *testing.T) {
	s := newFileSession(t)
	filters := []string{strings.Repeat("a", 101), strings.Repeat("日", 101), "a\x00b", "a\nb", "a\rb", "a\tb", "\x1b[31m", "\x7f", "\xff"}
	for _, filter := range filters {
		rec := s.call("GET", "/api/logs?source=sshd&filter="+url.QueryEscape(filter), nil, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("filter %q: got %d, want 422", filter, rec.Code)
		}
	}
	if len(s.logs.calls) != 0 {
		t.Fatal("rejected filter reached the log reader")
	}
}

func TestLogsFilterNarrowsResults(t *testing.T) {
	s := newFileSession(t)
	rec := s.get(logsURL("sshd", "100", "no-such-text-anywhere"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"lines":[]`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestLogsFirstValueWinsForDuplicateParameters(t *testing.T) {
	s := newFileSession(t)
	if rec := s.get("/api/logs?source=sshd&source=evil"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := s.get("/api/logs?source=evil&source=sshd"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestLogReadFailureIsGeneric(t *testing.T) {
	s := newFileSession(t)
	s.logs.failWith = errors.New("journalctl: permission denied reading /var/log/journal/abc")
	rec := s.get("/api/logs?source=sshd")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "/var/log") || strings.Contains(body, "permission") {
		t.Fatalf("leaked: %s", body)
	}
	apiError(t, rec)
}
