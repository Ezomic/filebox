package alerts

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// RemindEvery is how long an open problem goes without an email before it gets a reminder.
const RemindEvery = 7 * 24 * time.Hour

const subjectPrefix = "[filebox] "

// Problem is one thing a check found wrong. Its texts name disks, checks and accounts, never a file.
type Problem struct {
	// Key identifies the problem across checks and restarts, for example "smart-failing:WD-123".
	Key     string `json:"key"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
	// Cleared is the sentence the email uses once the problem is over.
	Cleared string `json:"cleared"`
	// Easing keeps an alert that is already open but never opens one: the problem is not bad enough to start an
	// alert, or could not be measured this time, yet it is not over either. Without a Subject the alert keeps the
	// texts it has.
	Easing bool `json:"-"`
}

// Result is what one check found. Known is false when the check could not look, so its open alerts stay as they are.
type Result struct {
	Source   string
	Known    bool
	Problems []Problem
}

// Alert is an open problem, or one that is over while the email saying so is still owed.
type Alert struct {
	Problem
	Source  string    `json:"source"`
	Started time.Time `json:"started"`
	// Mailed is when the last start or reminder email went out, zero until the first one did.
	Mailed time.Time `json:"mailed,omitzero"`
	// Ended is when the problem was last seen to be over, zero while it is open.
	Ended time.Time `json:"ended,omitzero"`
}

type State struct {
	Open []Alert `json:"open"`
	// Closed holds the alerts that ended after their start email went out, until the email saying so does too.
	Closed []Alert `json:"closed"`
}

type Kind string

const (
	KindStarted  Kind = "started"
	KindReminder Kind = "reminder"
	KindCleared  Kind = "cleared"
)

type Mail struct {
	Kind    Kind
	Key     string
	Subject string
	Text    string
}

// Decide is the whole alert policy. From the alerts in state, the latest results and the time it returns the emails
// that are due and the state to keep. Sending is up to the caller, who records each email that went out with
// Delivered, so one that fails is due again on the next call. An alert whose start email never went out ends without
// an email, because nobody heard of it. An alert from a source that is no longer checked at all ends as well.
func Decide(state State, results []Result, now time.Time) ([]Mail, State) {
	known := map[string]bool{}
	for _, result := range results {
		known[result.Source] = result.Known
	}
	open, closed := byKey(state.Open), byKey(state.Closed)

	next := map[string]Alert{}
	for _, result := range results {
		if !result.Known {
			continue
		}
		for _, problem := range result.Problems {
			if alert, ok := carried(open, closed, problem, now); ok {
				delete(closed, problem.Key)
				next[problem.Key] = update(alert, problem, result.Source)
			}
		}
	}
	for key, alert := range open {
		if _, still := next[key]; still {
			continue
		}
		if checked, reported := known[alert.Source]; reported && !checked {
			next[key] = alert
			continue
		}
		if !alert.Mailed.IsZero() {
			alert.Ended = now
			closed[key] = alert
		}
	}

	decided := State{Open: sortAlerts(next), Closed: sortAlerts(closed)}
	var mails []Mail
	for _, alert := range decided.Open {
		switch {
		case alert.Mailed.IsZero():
			mails = append(mails, startedMail(alert))
		case now.Sub(alert.Mailed) >= RemindEvery:
			mails = append(mails, reminderMail(alert))
		}
	}
	for _, alert := range decided.Closed {
		mails = append(mails, clearedMail(alert))
	}

	return mails, decided
}

// carried finds the alert a reported problem belongs to. A problem that comes back before the email saying it was
// over went out picks up its old alert again, so that email is never sent.
func carried(open, closed map[string]Alert, problem Problem, now time.Time) (Alert, bool) {
	if alert, ok := open[problem.Key]; ok {
		return alert, true
	}
	if problem.Easing {
		return Alert{}, false
	}
	if alert, ok := closed[problem.Key]; ok {
		alert.Ended = time.Time{}
		return alert, true
	}

	return Alert{Started: now}, true
}

func update(alert Alert, problem Problem, source string) Alert {
	alert.Source = source
	if problem.Subject == "" {
		return alert
	}
	problem.Easing = false
	alert.Problem = problem

	return alert
}

// Delivered is the state after mail went out at now.
func Delivered(state State, mail Mail, now time.Time) State {
	next := State{Open: slices.Clone(state.Open), Closed: slices.Clone(state.Closed)}
	if mail.Kind == KindCleared {
		next.Closed = slices.DeleteFunc(next.Closed, func(alert Alert) bool { return alert.Key == mail.Key })
		return next
	}
	for i := range next.Open {
		if next.Open[i].Key == mail.Key {
			next.Open[i].Mailed = now
		}
	}

	return next
}

func byKey(alerts []Alert) map[string]Alert {
	keyed := make(map[string]Alert, len(alerts))
	for _, alert := range alerts {
		keyed[alert.Key] = alert
	}

	return keyed
}

func sortAlerts(keyed map[string]Alert) []Alert {
	alerts := make([]Alert, 0, len(keyed))
	for _, alert := range keyed {
		alerts = append(alerts, alert)
	}
	slices.SortFunc(alerts, func(a, b Alert) int {
		return cmp.Or(a.Started.Compare(b.Started), cmp.Compare(a.Key, b.Key))
	})

	return alerts
}

func startedMail(alert Alert) Mail {
	return Mail{
		Kind: KindStarted, Key: alert.Key, Subject: subjectPrefix + "Problem: " + alert.Subject,
		Text: fmt.Sprintf("%s\n\n%s\n\nFound on %s. You get a reminder every 7 days while it lasts, and an email when it is over. "+
			"The dashboard of the admin panel lists every open problem.\n", alert.Subject, alert.Detail, when(alert.Started)),
	}
}

func reminderMail(alert Alert) Mail {
	return Mail{
		Kind: KindReminder, Key: alert.Key, Subject: subjectPrefix + "Still a problem: " + alert.Subject,
		Text: fmt.Sprintf("%s\n\n%s\n\nThis has been going on since %s. You get a reminder every 7 days while it lasts, "+
			"and an email when it is over.\n", alert.Subject, alert.Detail, when(alert.Started)),
	}
}

func clearedMail(alert Alert) Mail {
	return Mail{
		Kind: KindCleared, Key: alert.Key, Subject: subjectPrefix + "Resolved: " + alert.Subject,
		Text: fmt.Sprintf("%s\n\n%s\n\nFound on %s, over on %s.\n", alert.Subject, alert.Cleared, when(alert.Started), when(alert.Ended)),
	}
}

func when(at time.Time) string {
	return at.Format("Mon 2 Jan 2006 15:04 MST")
}
