package alerts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/mail"
)

// switchable is a check whose findings the test sets.
type switchable struct {
	result Result
}

func (s *switchable) check(time.Time) Result {
	return s.result
}

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func openMonitor(t *testing.T, dir string, sender mail.Sender, checks ...Check) (*Monitor, *clock) {
	t.Helper()
	m, err := Open(dir, sender, checks)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{at: start}
	m.now = c.now

	return m, c
}

func subjects(messages []mail.Message) string {
	var names []string
	for _, message := range messages {
		names = append(names, message.Subject)
	}

	return strings.Join(names, " | ")
}

func TestMonitorMailsTheAddressAndRemembersAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	disk := &switchable{result: diskFull(96)}
	sender := &mail.Fake{}
	m, c := openMonitor(t, dir, sender, disk.check)
	if err := m.SetAddress("admin@example.com"); err != nil {
		t.Fatal(err)
	}

	m.Tick(context.Background())
	sent := sender.Sent()
	if len(sent) != 1 || sent[0].To != "admin@example.com" || sent[0].Subject != "[filebox] Problem: The data disk is full" {
		t.Fatalf("got %+v", sent)
	}
	if m.Healthy() {
		t.Fatal("healthy with a problem open")
	}

	restarted, rc := openMonitor(t, dir, sender, disk.check)
	rc.at = c.at.Add(time.Minute)
	restarted.Tick(context.Background())
	if len(sender.Sent()) != 1 {
		t.Fatalf("a restart mailed again: %s", subjects(sender.Sent()))
	}
	status := restarted.Status()
	if status.Address != "admin@example.com" || len(status.Open) != 1 || !status.Mail || !status.Checked.Equal(rc.at) {
		t.Fatalf("got %+v", status)
	}

	disk.result = fine("disk")
	rc.at = rc.at.Add(time.Hour)
	restarted.Tick(context.Background())
	if subjects(sender.Sent()[1:]) != "[filebox] Resolved: The data disk is full" || !restarted.Healthy() {
		t.Fatalf("got %s", subjects(sender.Sent()))
	}
}

func TestMonitorRetriesAFailedMailOnTheNextTick(t *testing.T) {
	disk := &switchable{result: diskFull(96)}
	sender := &mail.Fake{Err: mail.ErrUnavailable}
	m, c := openMonitor(t, t.TempDir(), sender, disk.check)
	_ = m.SetAddress("admin@example.com")

	m.Tick(context.Background())
	if len(sender.Sent()) != 0 || !m.Status().Open[0].Mailed.IsZero() {
		t.Fatal("a failed mail must not count as sent")
	}
	sender.Err = nil
	c.at = c.at.Add(10 * time.Minute)
	m.Tick(context.Background())
	if subjects(sender.Sent()) != "[filebox] Problem: The data disk is full" {
		t.Fatalf("got %s", subjects(sender.Sent()))
	}
	if !m.Status().Open[0].Started.Equal(start) {
		t.Fatal("the retry must keep when the problem started")
	}
}

// stopAfter fails every send after the first n.
type stopAfter struct {
	mail.Fake
	n int
}

func (s *stopAfter) Send(ctx context.Context, to, subject, text string) error {
	if len(s.Sent()) >= s.n {
		return mail.ErrRateLimited
	}

	return s.Fake.Send(ctx, to, subject, text)
}

func TestMonitorStopsAtTheFirstFailureAndSendsTheRestLater(t *testing.T) {
	smart := &switchable{result: Result{Source: "smart", Known: true, Problems: []Problem{
		{Key: "a", Subject: "A"}, {Key: "b", Subject: "B"}, {Key: "c", Subject: "C"},
	}}}
	sender := &stopAfter{n: 1}
	m, c := openMonitor(t, t.TempDir(), sender, smart.check)
	_ = m.SetAddress("admin@example.com")

	m.Tick(context.Background())
	if subjects(sender.Sent()) != "[filebox] Problem: A" {
		t.Fatalf("got %s", subjects(sender.Sent()))
	}
	sender.n = 10
	c.at = c.at.Add(10 * time.Minute)
	m.Tick(context.Background())
	if subjects(sender.Sent()) != "[filebox] Problem: A | [filebox] Problem: B | [filebox] Problem: C" {
		t.Fatalf("got %s", subjects(sender.Sent()))
	}
}

func TestMonitorWithoutAnAddressOrMailOnlyKeepsTrack(t *testing.T) {
	disk := &switchable{result: diskFull(96)}
	sender := &mail.Fake{}
	m, c := openMonitor(t, t.TempDir(), sender, disk.check)
	m.Tick(context.Background())
	if len(sender.Sent()) != 0 || len(m.Status().Open) != 1 {
		t.Fatalf("without an address nothing goes out, but the problem shows: %+v", m.Status())
	}

	_ = m.SetAddress("admin@example.com")
	c.at = c.at.Add(time.Hour)
	m.Tick(context.Background())
	if len(sender.Sent()) != 1 || !strings.Contains(sender.Sent()[0].Text, when(start)) {
		t.Fatalf("an address set later gets the open problems, with when they started: %+v", sender.Sent())
	}

	off, _ := openMonitor(t, t.TempDir(), nil, disk.check)
	_ = off.SetAddress("admin@example.com")
	off.Tick(context.Background())
	if status := off.Status(); status.Mail || len(status.Open) != 1 {
		t.Fatalf("got %+v", status)
	}
}

func TestMonitorSendsATestEmail(t *testing.T) {
	sender := &mail.Fake{}
	m, _ := openMonitor(t, t.TempDir(), sender)
	if err := m.SendTest(context.Background()); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("got %v", err)
	}
	_ = m.SetAddress("admin@example.com")
	if err := m.SendTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := sender.Sent(); len(sent) != 1 || sent[0].To != "admin@example.com" || sent[0].Subject != "[filebox] Test email" {
		t.Fatalf("got %+v", sent)
	}

	sender.Err = mail.ErrRejected
	if err := m.SendTest(context.Background()); !errors.Is(err, mail.ErrRejected) {
		t.Fatalf("got %v", err)
	}
	off, _ := openMonitor(t, t.TempDir(), nil)
	_ = off.SetAddress("admin@example.com")
	if err := off.SendTest(context.Background()); !errors.Is(err, ErrMailOff) {
		t.Fatalf("got %v", err)
	}
}

func TestMonitorStartsOverFromADamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := openMonitor(t, dir, nil)
	if status := m.Status(); status.Address != "" || len(status.Open) != 0 {
		t.Fatalf("got %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile+".bad")); err != nil {
		t.Fatalf("the damaged file is kept aside: %v", err)
	}
}

func TestMonitorFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	m, _ := openMonitor(t, dir, nil)
	if err := m.SetAddress("admin@example.com"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v", info.Mode())
	}
}
