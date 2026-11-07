package alerts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)

func diskFull(percent int) Result {
	return Result{Source: "disk", Known: true, Problems: []Problem{{
		Key: "disk-full", Subject: "The data disk is full", Detail: fmt.Sprintf("It is %d%% full.", percent),
		Cleared: "The data disk has room again.",
	}}}
}

func fine(source string) Result {
	return Result{Source: source, Known: true}
}

// deliverAll records every mail as sent, as a tick does when the mail service accepts them all.
func deliverAll(state State, mails []Mail, at time.Time) State {
	for _, mail := range mails {
		state = Delivered(state, mail, at)
	}

	return state
}

// gone runs Decide with results at at, when the problem first goes missing, and again ClearAfter later.
func gone(state State, results []Result, at time.Time) ([]Mail, State) {
	_, state = Decide(state, results, at)

	return Decide(state, results, at.Add(ClearAfter))
}

func kinds(mails []Mail) string {
	var names []string
	for _, mail := range mails {
		names = append(names, string(mail.Kind)+" "+mail.Key)
	}

	return strings.Join(names, ", ")
}

func TestANewProblemMailsOnceWhenItStarts(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	if kinds(mails) != "started disk-full" {
		t.Fatalf("got %q", kinds(mails))
	}
	if !strings.HasPrefix(mails[0].Subject, "[filebox] Problem: The data disk is full") || !strings.Contains(mails[0].Text, "It is 96% full.") {
		t.Fatalf("unexpected mail %+v", mails[0])
	}
	if len(state.Open) != 1 || !state.Open[0].Started.Equal(start) || !state.Open[0].Mailed.IsZero() {
		t.Fatalf("an alert is only marked mailed once the mail went out: %+v", state.Open)
	}
	state = deliverAll(state, mails, start)

	for _, later := range []time.Duration{time.Minute, time.Hour, 3 * 24 * time.Hour} {
		if again, _ := Decide(state, []Result{diskFull(97)}, start.Add(later)); len(again) != 0 {
			t.Fatalf("after %v: a lasting problem mailed again: %q", later, kinds(again))
		}
	}
}

func TestAnOpenProblemGetsAReminderEverySevenDays(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	if mails, _ := Decide(state, []Result{diskFull(96)}, start.Add(RemindEvery-time.Second)); len(mails) != 0 {
		t.Fatalf("a reminder before 7 days: %q", kinds(mails))
	}
	week := start.Add(RemindEvery)
	mails, state = Decide(state, []Result{diskFull(97)}, week)
	if kinds(mails) != "reminder disk-full" || !strings.HasPrefix(mails[0].Subject, "[filebox] Still a problem: ") {
		t.Fatalf("got %+v", mails)
	}
	if !strings.Contains(mails[0].Text, "It is 97% full.") || !strings.Contains(mails[0].Text, when(start)) {
		t.Fatalf("a reminder carries the latest detail and when it started: %q", mails[0].Text)
	}
	state = deliverAll(state, mails, week)

	if mails, _ := Decide(state, []Result{diskFull(97)}, week.Add(RemindEvery-time.Second)); len(mails) != 0 {
		t.Fatalf("the next reminder counts from the last one: %q", kinds(mails))
	}
	if mails, _ := Decide(state, []Result{diskFull(97)}, week.Add(RemindEvery)); kinds(mails) != "reminder disk-full" {
		t.Fatalf("no second reminder: %q", kinds(mails))
	}
}

func TestAProblemThatEndsMailsOnceThatItIsOver(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	over := start.Add(2 * time.Hour)
	mails, state = Decide(state, []Result{fine("disk")}, over)
	if len(mails) != 0 || len(state.Open) != 1 || !state.Open[0].Gone.Equal(over) {
		t.Fatalf("an alert waits an hour before it ends: %q %+v", kinds(mails), state.Open)
	}
	if mails, _ := Decide(state, []Result{fine("disk")}, over.Add(ClearAfter-time.Second)); len(mails) != 0 {
		t.Fatalf("ended within the hour: %q", kinds(mails))
	}
	mails, state = Decide(state, []Result{fine("disk")}, over.Add(ClearAfter))
	if kinds(mails) != "cleared disk-full" || !strings.HasPrefix(mails[0].Subject, "[filebox] Resolved: The data disk is full") {
		t.Fatalf("got %+v", mails)
	}
	if !strings.Contains(mails[0].Text, "The data disk has room again.") || !strings.Contains(mails[0].Text, when(over)) {
		t.Fatalf("unexpected text %q", mails[0].Text)
	}
	if len(state.Open) != 0 || len(state.Closed) != 1 {
		t.Fatalf("got %+v", state)
	}
	state = deliverAll(state, mails, over.Add(ClearAfter))
	if len(state.Closed) != 0 {
		t.Fatalf("a sent mail is not owed any more: %+v", state.Closed)
	}
	if mails, _ := Decide(state, []Result{fine("disk")}, over.Add(2*ClearAfter)); len(mails) != 0 {
		t.Fatalf("mailed twice that it is over: %q", kinds(mails))
	}
}

func TestAProblemNobodyWasToldAboutEndsQuietly(t *testing.T) {
	_, state := Decide(State{}, []Result{diskFull(96)}, start)
	mails, state := gone(state, []Result{fine("disk")}, start.Add(time.Hour))
	if len(mails) != 0 || len(state.Open) != 0 || len(state.Closed) != 0 {
		t.Fatalf("got %q and %+v", kinds(mails), state)
	}
}

func TestARestartSendsNoDuplicates(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	saved, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restarted State
	if err := json.Unmarshal(saved, &restarted); err != nil {
		t.Fatal(err)
	}
	if mails, _ := Decide(restarted, []Result{diskFull(96)}, start.Add(time.Minute)); len(mails) != 0 {
		t.Fatalf("a restart mailed again: %q", kinds(mails))
	}
	if mails, _ := Decide(restarted, []Result{diskFull(96)}, start.Add(RemindEvery)); kinds(mails) != "reminder disk-full" {
		t.Fatalf("the reminder survives a restart: %q", kinds(mails))
	}
}

func TestSeveralProblemsAtOnceEachGetTheirOwnMail(t *testing.T) {
	smart := Result{Source: "smart", Known: true, Problems: []Problem{
		{Key: "smart-failing:B", Subject: "Disk /dev/sdb is failing", Detail: "b", Cleared: "ok"},
		{Key: "smart-failing:A", Subject: "Disk /dev/sda is failing", Detail: "a", Cleared: "ok"},
	}}
	mails, state := Decide(State{}, []Result{diskFull(96), smart}, start)
	if kinds(mails) != "started disk-full, started smart-failing:A, started smart-failing:B" {
		t.Fatalf("got %q", kinds(mails))
	}

	state = Delivered(state, mails[0], start)
	state = Delivered(state, mails[1], start)
	smart.Problems = smart.Problems[:1]
	mails, state = Decide(state, []Result{fine("disk"), smart}, start.Add(time.Hour))
	if kinds(mails) != "started smart-failing:B" {
		t.Fatalf("each alert follows its own course: %q", kinds(mails))
	}
	mails, state = Decide(state, []Result{fine("disk"), smart}, start.Add(time.Hour+ClearAfter))
	if kinds(mails) != "started smart-failing:B, cleared disk-full, cleared smart-failing:A" {
		t.Fatalf("each alert follows its own course: %q", kinds(mails))
	}
	if len(state.Open) != 1 || state.Open[0].Key != "smart-failing:B" {
		t.Fatalf("got %+v", state.Open)
	}
}

func TestAnUnsentMailStaysDue(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	again, state := Decide(state, []Result{diskFull(96)}, start.Add(10*time.Minute))
	if kinds(again) != kinds(mails) || !state.Open[0].Started.Equal(start) {
		t.Fatalf("a start mail that was not sent, for example without an address, stays due: %q", kinds(again))
	}
}

func TestAFailedMailWaitsLongerEachTime(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	at := start
	waits := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for _, wait := range waits {
		state = Failed(state, mails[0], at)
		if early, _ := Decide(state, []Result{diskFull(96)}, at.Add(wait-time.Second)); len(early) != 0 {
			t.Fatalf("retried before %v: %q", wait, kinds(early))
		}
		at = at.Add(wait)
		if mails, _ = Decide(state, []Result{diskFull(96)}, at); kinds(mails) != "started disk-full" {
			t.Fatalf("not retried after %v: %q", wait, kinds(mails))
		}
	}
	state = Delivered(state, mails[0], at)
	if state.Open[0].Failures != 0 || !state.Open[0].RetryAt.IsZero() || !state.Open[0].Mailed.Equal(at) {
		t.Fatalf("a mail that went out starts the count over: %+v", state.Open[0])
	}

	mails, state = gone(state, []Result{fine("disk")}, at.Add(time.Hour))
	failedAt := at.Add(2 * time.Hour)
	state = Failed(state, mails[0], failedAt)
	if again, _ := Decide(state, []Result{fine("disk")}, failedAt.Add(5*time.Minute)); len(again) != 0 {
		t.Fatalf("a failed cleared mail waits as well: %q", kinds(again))
	}
	again, _ := Decide(state, []Result{fine("disk")}, failedAt.Add(firstRetry))
	if kinds(again) != "cleared disk-full" || again[0].Text != mails[0].Text {
		t.Fatalf("and is then sent unchanged: %q", kinds(again))
	}
}

func TestOneFailedMailDoesNotHoldBackTheOthers(t *testing.T) {
	smart := Result{Source: "smart", Known: true, Problems: []Problem{{Key: "a", Subject: "A"}, {Key: "b", Subject: "B"}}}
	mails, state := Decide(State{}, []Result{smart}, start)
	state = Failed(state, mails[0], start)
	state = Delivered(state, mails[1], start)
	if mails, _ := Decide(state, []Result{smart}, start.Add(time.Minute)); len(mails) != 0 {
		t.Fatalf("got %q", kinds(mails))
	}
	smart.Problems = append(smart.Problems, Problem{Key: "c", Subject: "C"})
	if mails, _ := Decide(state, []Result{smart}, start.Add(2*time.Minute)); kinds(mails) != "started c" {
		t.Fatalf("a new alert goes out while another one waits: %q", kinds(mails))
	}
}

func TestAProblemThatComesAndGoesIsOneAlert(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)
	at := start
	for range 5 {
		at = at.Add(10 * time.Minute)
		if mails, state = Decide(state, []Result{fine("disk")}, at); len(mails) != 0 {
			t.Fatalf("gone for a moment: %q", kinds(mails))
		}
		at = at.Add(50 * time.Minute)
		if mails, state = Decide(state, []Result{diskFull(96)}, at); len(mails) != 0 {
			t.Fatalf("back within the hour: %q", kinds(mails))
		}
	}
	if len(state.Open) != 1 || !state.Open[0].Started.Equal(start) || !state.Open[0].Gone.IsZero() {
		t.Fatalf("the flapping problem is still the first alert: %+v", state.Open)
	}

	away := at.Add(10 * time.Minute)
	mails, state = gone(state, []Result{fine("disk")}, away)
	if kinds(mails) != "cleared disk-full" || !state.Closed[0].Ended.Equal(away) {
		t.Fatalf("once away for an hour it is over, from when it went: %q %+v", kinds(mails), state.Closed)
	}
}

func TestACheckThatCannotLookForADayIsAProblem(t *testing.T) {
	unknown := Result{Source: "smart", Name: "the SMART disk health"}
	mails, state := Decide(State{}, []Result{unknown}, start)
	if len(mails) != 0 || state.Unchecked["smart"] != (Unchecked{Name: "the SMART disk health", Since: start}) {
		t.Fatalf("got %q %+v", kinds(mails), state.Unchecked)
	}
	_, state = Decide(state, []Result{unknown}, start.Add(UncheckedAfter-time.Minute))
	if len(state.Open) != 0 || !state.Unchecked["smart"].Since.Equal(start) {
		t.Fatalf("since stays when it started: %+v", state)
	}

	mails, state = Decide(state, []Result{unknown}, start.Add(UncheckedAfter))
	if kinds(mails) != "started unchecked:smart" || mails[0].Subject != "[filebox] Problem: Could not check the SMART disk health since "+when(start) {
		t.Fatalf("got %+v", mails)
	}
	state = deliverAll(state, mails, start.Add(UncheckedAfter))

	saved, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restarted State
	if err := json.Unmarshal(saved, &restarted); err != nil {
		t.Fatal(err)
	}
	mails, state = gone(restarted, []Result{fine("smart")}, start.Add(UncheckedAfter+time.Hour))
	if kinds(mails) != "cleared unchecked:smart" || len(state.Unchecked) != 0 || !strings.Contains(mails[0].Text, "Filebox can check the SMART disk health again.") {
		t.Fatalf("got %q %+v", kinds(mails), state)
	}
}

func TestAShortSpellOfNotLookingStartsOver(t *testing.T) {
	unknown := Result{Source: "smart", Name: "the SMART disk health"}
	_, state := Decide(State{}, []Result{unknown}, start)
	_, state = Decide(state, []Result{fine("smart")}, start.Add(12*time.Hour))
	_, state = Decide(state, []Result{unknown}, start.Add(13*time.Hour))
	if mails, state := Decide(state, []Result{unknown}, start.Add(UncheckedAfter+time.Hour)); len(mails) != 0 || len(state.Open) != 0 {
		t.Fatalf("24 hours means 24 hours in a row: %q", kinds(mails))
	}
}

func TestACheckThatCouldNotLookKeepsItsAlerts(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	mails, state = Decide(state, []Result{{Source: "disk"}}, start.Add(time.Hour))
	if len(mails) != 0 || len(state.Open) != 1 {
		t.Fatalf("an unknown result must neither clear nor mail: %q %+v", kinds(mails), state)
	}
	if mails, _ := Decide(state, []Result{{Source: "disk"}}, start.Add(RemindEvery)); kinds(mails) != "reminder disk-full, started unchecked:disk" {
		t.Fatalf("a held alert still gets its reminder, and a week of not looking is a problem of its own: %q", kinds(mails))
	}
}

func TestAnAlertFromASourceThatIsNoLongerCheckedEnds(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	mails, state = gone(state, []Result{fine("smart")}, start.Add(time.Hour))
	if kinds(mails) != "cleared disk-full" || len(state.Open) != 0 {
		t.Fatalf("got %q %+v", kinds(mails), state)
	}
}

func TestAnEasingProblemKeepsAnAlertButNeverStartsOne(t *testing.T) {
	easing := func(detail string) Result {
		return Result{Source: "disk", Known: true, Problems: []Problem{{Key: "disk-full", Subject: "Almost full", Detail: detail, Easing: true}}}
	}
	if mails, state := Decide(State{}, []Result{easing("92%")}, start); len(mails) != 0 || len(state.Open) != 0 {
		t.Fatalf("an easing problem opened an alert: %q %+v", kinds(mails), state)
	}

	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)
	mails, state = Decide(state, []Result{easing("92%")}, start.Add(time.Hour))
	if len(mails) != 0 || len(state.Open) != 1 || state.Open[0].Detail != "92%" {
		t.Fatalf("an easing problem keeps the alert with its new texts: %q %+v", kinds(mails), state.Open)
	}

	textless := Result{Source: "disk", Known: true, Problems: []Problem{{Key: "disk-full", Easing: true}}}
	_, state = Decide(state, []Result{textless}, start.Add(2*time.Hour))
	if len(state.Open) != 1 || state.Open[0].Subject != "Almost full" || state.Open[0].Detail != "92%" {
		t.Fatalf("an easing problem without texts keeps the old ones: %+v", state.Open)
	}
}

func TestAProblemThatReturnsBeforeItsClearedMailWentOutCarriesOn(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	_, state = gone(state, []Result{fine("disk")}, start.Add(time.Hour))
	if len(state.Closed) != 1 {
		t.Fatalf("got %+v", state)
	}
	mails, state = Decide(state, []Result{diskFull(97)}, start.Add(3*time.Hour))
	if len(mails) != 0 {
		t.Fatalf("nobody heard it was over, so nothing to say: %q", kinds(mails))
	}
	if len(state.Closed) != 0 || len(state.Open) != 1 || !state.Open[0].Started.Equal(start) || !state.Open[0].Ended.IsZero() {
		t.Fatalf("the old alert carries on: %+v", state)
	}

	_, state = gone(state, []Result{fine("disk")}, start.Add(4*time.Hour))
	easing := Result{Source: "disk", Known: true, Problems: []Problem{{Key: "disk-full", Easing: true}}}
	if mails, _ := Decide(state, []Result{easing}, start.Add(6*time.Hour)); kinds(mails) != "cleared disk-full" {
		t.Fatalf("an easing problem does not bring an ended alert back: %q", kinds(mails))
	}
}

func TestMailsAreSortedAndDecideDoesNotChangeItsInput(t *testing.T) {
	early := Result{Source: "smart", Known: true, Problems: []Problem{{Key: "z", Subject: "Z"}}}
	_, state := Decide(State{}, []Result{early}, start)
	before, _ := json.Marshal(state)
	mails, next := Decide(state, []Result{early, diskFull(96)}, start.Add(time.Minute))
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("Decide changed the state it was given")
	}
	if kinds(mails) != "started z, started disk-full" || next.Open[0].Key != "z" {
		t.Fatalf("oldest first: %q", kinds(mails))
	}
	delivered := Delivered(next, mails[0], start)
	if !next.Open[0].Mailed.IsZero() || delivered.Open[0].Mailed.IsZero() {
		t.Fatal("Delivered must return a new state and leave its input alone")
	}
}
