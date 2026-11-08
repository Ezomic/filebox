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
	if len(sender.Sent()) != 1 || restarted.Healthy() {
		t.Fatalf("an alert stays open for an hour after the problem went: %s", subjects(sender.Sent()))
	}
	rc.at = rc.at.Add(ClearAfter)
	restarted.Tick(context.Background())
	if subjects(sender.Sent()[1:]) != "[filebox] Resolved: The data disk is full" || !restarted.Healthy() {
		t.Fatalf("got %s", subjects(sender.Sent()))
	}
}

// refusing fails the emails whose subject it lists.
type refusing struct {
	mail.Fake
	refuse map[string]bool
	tries  []string
}

func (r *refusing) Send(ctx context.Context, to, subject, text string) error {
	r.tries = append(r.tries, subject)
	if r.refuse[subject] {
		return mail.ErrRejected
	}

	return r.Fake.Send(ctx, to, subject, text)
}

func TestMonitorSendsEachMailOnItsOwnAndBacksOff(t *testing.T) {
	smart := &switchable{result: Result{Source: "smart", Known: true, Problems: []Problem{
		{Key: "a", Subject: "A"}, {Key: "b", Subject: "B"}, {Key: "c", Subject: "C"},
	}}}
	sender := &refusing{refuse: map[string]bool{"[filebox] Problem: A": true}}
	m, c := openMonitor(t, t.TempDir(), sender, smart.check)
	_ = m.SetAddress("admin@example.com")

	m.Tick(context.Background())
	if subjects(sender.Sent()) != "[filebox] Problem: B | [filebox] Problem: C" {
		t.Fatalf("one refused email must not hold back the others: %s", subjects(sender.Sent()))
	}
	if open := m.Status().Open[0]; open.Key != "a" || !open.Mailed.IsZero() || open.Failures != 1 || !open.RetryAt.Equal(start.Add(firstRetry)) {
		t.Fatalf("got %+v", open)
	}

	c.at = start.Add(5 * time.Minute)
	m.Tick(context.Background())
	if len(sender.tries) != 3 {
		t.Fatalf("tried again before the wait was over: %v", sender.tries)
	}
	c.at = start.Add(firstRetry)
	m.Tick(context.Background())
	if len(sender.tries) != 4 || m.Status().Open[0].RetryAt != c.at.Add(2*firstRetry) {
		t.Fatalf("the second failure waits twice as long: %v %+v", sender.tries, m.Status().Open[0])
	}

	delete(sender.refuse, "[filebox] Problem: A")
	c.at = c.at.Add(2 * firstRetry)
	m.Tick(context.Background())
	if subjects(sender.Sent()) != "[filebox] Problem: B | [filebox] Problem: C | [filebox] Problem: A" || m.Status().Open[0].Failures != 0 {
		t.Fatalf("got %s %+v", subjects(sender.Sent()), m.Status().Open[0])
	}
	if !m.Status().Open[0].Started.Equal(start) {
		t.Fatal("the retry must keep when the problem started")
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

func TestMonitorStartsOverFromADamagedFileButKeepsTheAddress(t *testing.T) {
	dir := t.TempDir()
	disk := &switchable{result: diskFull(96)}
	m, _ := openMonitor(t, dir, &mail.Fake{}, disk.check)
	if err := m.SetAddress("admin@example.com"); err != nil {
		t.Fatal(err)
	}
	m.Tick(context.Background())
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, _ := openMonitor(t, dir, &mail.Fake{})
	if status := reopened.Status(); status.Address != "admin@example.com" || len(status.Open) != 0 {
		t.Fatalf("a damaged state file only loses the state: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile+".bad")); err != nil {
		t.Fatalf("the damaged file is kept aside: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte("nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := (func() Status { m, _ := openMonitor(t, dir, nil); return m.Status() })(); status.Address != "" {
		t.Fatalf("a damaged settings file loses only the address: %+v", status)
	}
}

func TestMonitorShowsTheChecksThatCouldNotLook(t *testing.T) {
	disk := &switchable{result: Result{Source: "disk", Name: "how full the data disk is"}}
	smart := &switchable{result: Result{Source: "smart", Name: "the SMART disk health"}}
	m, c := openMonitor(t, t.TempDir(), &mail.Fake{}, disk.check, smart.check)
	m.Tick(context.Background())
	status := m.Status()
	if len(status.Unchecked) != 2 || status.Unchecked[0].Name != "how full the data disk is" || !status.Unchecked[1].Since.Equal(start) {
		t.Fatalf("got %+v", status.Unchecked)
	}
	if !m.Healthy() {
		t.Fatal("not being able to look for a while is shown, but is no alert yet")
	}

	disk.result = fine("disk")
	c.at = start.Add(UncheckedAfter)
	m.Tick(context.Background())
	status = m.Status()
	if len(status.Unchecked) != 1 || len(status.Open) != 1 || status.Open[0].Key != "unchecked:smart" || m.Healthy() {
		t.Fatalf("a day without looking is an alert: %+v", status)
	}
}

func TestMonitorFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	m, _ := openMonitor(t, dir, nil)
	if err := m.SetAddress("admin@example.com"); err != nil {
		t.Fatal(err)
	}
	m.Tick(context.Background())
	for _, name := range []string{stateFile, settingsFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s: mode %v", name, info.Mode())
		}
	}
}
