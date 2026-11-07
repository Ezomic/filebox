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
	if kinds(mails) != "cleared disk-full" || !strings.HasPrefix(mails[0].Subject, "[filebox] Resolved: The data disk is full") {
		t.Fatalf("got %+v", mails)
	}
	if !strings.Contains(mails[0].Text, "The data disk has room again.") || !strings.Contains(mails[0].Text, when(over)) {
		t.Fatalf("unexpected text %q", mails[0].Text)
	}
	if len(state.Open) != 0 || len(state.Closed) != 1 {
		t.Fatalf("got %+v", state)
	}
	state = deliverAll(state, mails, over)
	if len(state.Closed) != 0 {
		t.Fatalf("a sent mail is not owed any more: %+v", state.Closed)
	}
	if mails, _ := Decide(state, []Result{fine("disk")}, over.Add(time.Hour)); len(mails) != 0 {
		t.Fatalf("mailed twice that it is over: %q", kinds(mails))
	}
}

func TestAProblemNobodyWasToldAboutEndsQuietly(t *testing.T) {
	_, state := Decide(State{}, []Result{diskFull(96)}, start)
	mails, state := Decide(state, []Result{fine("disk")}, start.Add(time.Hour))
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
	if kinds(mails) != "started smart-failing:B, cleared disk-full, cleared smart-failing:A" {
		t.Fatalf("each alert follows its own course: %q", kinds(mails))
	}
	if len(state.Open) != 1 || state.Open[0].Key != "smart-failing:B" {
		t.Fatalf("got %+v", state.Open)
	}
}

func TestAFailedMailIsDueAgainOnTheNextTick(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	again, state := Decide(state, []Result{diskFull(96)}, start.Add(10*time.Minute))
	if kinds(again) != kinds(mails) {
		t.Fatalf("an unsent start mail must be retried: %q", kinds(again))
	}
	if !state.Open[0].Started.Equal(start) {
		t.Fatalf("the retry keeps when it started: %+v", state.Open[0])
	}
	state = deliverAll(state, again, start.Add(10*time.Minute))

	mails, state = Decide(state, []Result{fine("disk")}, start.Add(time.Hour))
	again, _ = Decide(state, []Result{fine("disk")}, start.Add(2*time.Hour))
	if kinds(again) != "cleared disk-full" || again[0].Text != mails[0].Text {
		t.Fatalf("an unsent cleared mail must be retried unchanged: %q", kinds(again))
	}

	mails, state = Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)
	week := start.Add(RemindEvery)
	if mails, _ := Decide(state, []Result{diskFull(96)}, week); kinds(mails) != "reminder disk-full" {
		t.Fatalf("got %q", kinds(mails))
	}
	if mails, _ := Decide(state, []Result{diskFull(96)}, week.Add(10*time.Minute)); kinds(mails) != "reminder disk-full" {
		t.Fatalf("an unsent reminder must be retried: %q", kinds(mails))
	}
}

func TestACheckThatCouldNotLookKeepsItsAlerts(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	mails, state = Decide(state, []Result{{Source: "disk"}}, start.Add(time.Hour))
	if len(mails) != 0 || len(state.Open) != 1 {
		t.Fatalf("an unknown result must neither clear nor mail: %q %+v", kinds(mails), state)
	}
	if mails, _ := Decide(state, []Result{{Source: "disk"}}, start.Add(RemindEvery)); kinds(mails) != "reminder disk-full" {
		t.Fatalf("a held alert still gets its reminder: %q", kinds(mails))
	}
}

func TestAnAlertFromASourceThatIsNoLongerCheckedEnds(t *testing.T) {
	mails, state := Decide(State{}, []Result{diskFull(96)}, start)
	state = deliverAll(state, mails, start)

	mails, state = Decide(state, []Result{fine("smart")}, start.Add(time.Hour))
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

	_, state = Decide(state, []Result{fine("disk")}, start.Add(time.Hour))
	mails, state = Decide(state, []Result{diskFull(97)}, start.Add(2*time.Hour))
	if len(mails) != 0 {
		t.Fatalf("nobody heard it was over, so nothing to say: %q", kinds(mails))
	}
	if len(state.Closed) != 0 || len(state.Open) != 1 || !state.Open[0].Started.Equal(start) || !state.Open[0].Ended.IsZero() {
		t.Fatalf("the old alert carries on: %+v", state)
	}

	_, state = Decide(state, []Result{fine("disk")}, start.Add(3*time.Hour))
	easing := Result{Source: "disk", Known: true, Problems: []Problem{{Key: "disk-full", Easing: true}}}
	if mails, _ := Decide(state, []Result{easing}, start.Add(4*time.Hour)); kinds(mails) != "cleared disk-full" {
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
