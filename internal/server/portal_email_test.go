package server

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/mail"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	testPortalURL   = "https://files.example.com"
	resetToPassword = "a brand new password"
	genericAnswer   = "if that account exists and has a confirmed address, we sent an email"
)

var tokenInMail = regexp.MustCompile(`#([A-Za-z0-9_-]{43})`)

type emailHarness struct {
	*portalHarness
	mail     *mail.Fake
	contacts *auth.Contacts
	clock    *time.Time
	dir      string
}

func newEmailHarness(t *testing.T) *emailHarness {
	t.Helper()
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword, "cleo": cleoPassword}}
	dir := t.TempDir()
	roles, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.Promote("amy"); err != nil {
		t.Fatal(err)
	}
	contacts, err := auth.OpenContacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	spy := &spyUsers{FakeUsers: system.NewFakeUsers("amy", "bob", "cleo")}
	sender := &mail.Fake{}
	portal := NewPortal(PortalDeps{Verifier: verifier, Users: spy, Roles: roles, Contacts: contacts, Mail: sender, PortalURL: testPortalURL})
	portal.email.background = func(work func()) { work() }
	clock := time.Now()
	portal.email.tokens.now = func() time.Time { return clock }

	return &emailHarness{
		portalHarness: &portalHarness{portal: portal, verifier: verifier, roles: roles, spy: spy},
		mail:          sender,
		contacts:      contacts,
		clock:         &clock,
		dir:           dir,
	}
}

func (h *emailHarness) forgot(username, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/forgot", `{"username":`+quote(username)+`}`, ip, nil)
}

func (h *emailHarness) reset(token, password, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/reset", `{"token":`+quote(token)+`,"password":`+quote(password)+`}`, ip, nil)
}

func (h *emailHarness) confirm(token, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/email/confirm", `{"token":`+quote(token)+`}`, ip, nil)
}

func (h *emailHarness) setAddress(cookie *http.Cookie, address, current string) *httptest.ResponseRecorder {
	return h.do("PUT", "/api/me/email", `{"email":`+quote(address)+`,"current":`+quote(current)+`}`, "192.0.2.10", cookie)
}

func (h *emailHarness) lastToken(t *testing.T) string {
	t.Helper()
	sent := h.mail.Sent()
	if len(sent) == 0 {
		t.Fatal("no email was sent")
	}
	match := tokenInMail.FindStringSubmatch(sent[len(sent)-1].Text)
	if match == nil {
		t.Fatalf("no link in %q", sent[len(sent)-1].Text)
	}

	return match[1]
}

func (h *emailHarness) confirmedAddress(t *testing.T, user, address string) {
	t.Helper()
	if err := h.contacts.Set(user, address); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.contacts.Confirm(user, address); !ok || err != nil {
		t.Fatalf("confirm %v %v", ok, err)
	}
}

func (h *emailHarness) requestReset(t *testing.T, user, ip string) string {
	t.Helper()
	before := len(h.mail.Sent())
	if rec := h.forgot(user, ip); rec.Code != http.StatusOK {
		t.Fatalf("forgot got %d", rec.Code)
	}
	if len(h.mail.Sent()) != before+1 {
		t.Fatalf("expected one new email, have %d", len(h.mail.Sent())-before)
	}

	return h.lastToken(t)
}

func (h *emailHarness) addressState(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	rec := h.do("GET", "/api/me/email", "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	return rec.Body.String()
}

func TestSettingAnAddressEmailsAConfirmationLinkAndStaysUnconfirmed(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)

	if rec := h.setAddress(bob, "  Bob@Example.com ", bobPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	sent := h.mail.Sent()
	if len(sent) != 1 || sent[0].To != "Bob@Example.com" || !strings.Contains(sent[0].Text, testPortalURL+"/confirm-email#") {
		t.Fatalf("unexpected mail %+v", sent)
	}
	state := h.addressState(t, bob)
	if !strings.Contains(state, `"email":"Bob@Example.com"`) || !strings.Contains(state, `"confirmed":false`) {
		t.Fatalf("the address must be shown as typed and unconfirmed: %s", state)
	}
	if rec := h.forgot("bob", "192.0.2.20"); rec.Code != http.StatusOK || len(h.mail.Sent()) != 1 {
		t.Fatal("an unconfirmed address must not be usable for reset")
	}
}

func TestSettingAnAddressNeedsASessionAndTheCurrentPassword(t *testing.T) {
	h := newEmailHarness(t)
	if rec := h.setAddress(nil, "bob@example.com", bobPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/me/email", "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	bob := h.signedIn(t, "bob", bobPassword)
	if rec := h.setAddress(bob, "bob@example.com", "wrong password here"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if len(h.mail.Sent()) != 0 {
		t.Fatal("nothing may be sent without the password")
	}
	if _, found := h.contacts.Get("bob"); found {
		t.Fatal("nothing may be stored without the password")
	}
}

func TestAddressValidation(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)

	bad := map[string]string{
		"empty":             "",
		"spaces only":       "   ",
		"no at":             "bob.example.com",
		"two ats":           "a@b@example.com",
		"display name":      "Bob <bob@example.com>",
		"trailing dot":      "bob@example.com.",
		"leading dot local": ".bob@example.com",
		"double dot local":  "b..ob@example.com",
		"no domain dot":     "bob@localhost",
		"empty label":       "bob@example..com",
		"hyphen label":      "bob@-example.com",
		"comma list":        "a@example.com,b@example.com",
		"semicolon list":    "a@example.com;b@example.com",
		"inner space":       "bob smith@example.com",
		"newline":           "bob@example.com\nBcc: evil@example.com",
		"carriage return":   "bob@example.com\rBcc: evil@example.com",
		"inner tab":         "bob\t@example.com",
		"nul":               "bob@example.com\x00",
		"idn domain":        "bob@bücher.example",
		"unicode local":     "bøb@example.com",
		"fullwidth at":      "bob＠example.com",
		"very long":         strings.Repeat("a", 250) + "@example.com",
		"long local":        strings.Repeat("a", 65) + "@example.com",
		"quoted":            `"bob"@example.com`,
		"angle":             "<bob@example.com>",
	}
	for name, address := range bad {
		if rec := h.setAddress(bob, address, bobPassword); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d, want 422", name, rec.Code)
		}
	}
	if len(h.mail.Sent()) != 0 {
		t.Fatal("no mail may go to an invalid address")
	}

	for _, address := range []string{"bob@example.com", "Bob.Smith+files@Example.CO.uk", "b_o-b@sub.example.org"} {
		if rec := h.setAddress(bob, address, bobPassword); rec.Code != http.StatusNoContent {
			t.Errorf("%q: got %d", address, rec.Code)
		}
	}
}

func TestConfirmationLinkConfirmsOnceAndOnlyForItsUser(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	_ = h.setAddress(bob, "bob@example.com", bobPassword)
	token := h.lastToken(t)

	if rec := h.confirm(token, "192.0.2.30"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(h.addressState(t, bob), `"confirmed":true`) {
		t.Fatal("not confirmed")
	}
	if rec := h.confirm(token, "192.0.2.30"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a link must work once, got %d", rec.Code)
	}
	if _, found := h.contacts.Get("amy"); found {
		t.Fatal("another user must be untouched")
	}
}

func TestConfirmationNeedsNoSessionButCannotConfirmSomeoneElsesAddress(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	amy := h.signedIn(t, "amy", amyPassword)
	_ = h.setAddress(bob, "shared@example.com", bobPassword)
	bobToken := h.lastToken(t)
	_ = h.setAddress(amy, "amy@example.com", amyPassword)

	h.confirm(bobToken, "192.0.2.30")
	if got, _ := h.contacts.Get("amy"); got.Confirmed {
		t.Fatal("bob's link confirmed amy's address")
	}
	if got, _ := h.contacts.Get("bob"); !got.Confirmed {
		t.Fatal("bob's link must confirm bob's address")
	}
}

func TestConfirmationLinkExpires(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	_ = h.setAddress(bob, "bob@example.com", bobPassword)
	token := h.lastToken(t)

	*h.clock = h.clock.Add(confirmLinkTTL + time.Second)
	if rec := h.confirm(token, "192.0.2.30"); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
	if got, _ := h.contacts.Get("bob"); got.Confirmed {
		t.Fatal("an expired link confirmed")
	}
}

func TestChangingTheAddressKillsOutstandingLinks(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	h.confirmedAddress(t, "bob", "old@example.com")
	resetToken := h.requestReset(t, "bob", "192.0.2.40")

	_ = h.setAddress(bob, "new@example.com", bobPassword)
	confirmToken := h.lastToken(t)
	_ = h.setAddress(bob, "newer@example.com", bobPassword)

	if rec := h.confirm(confirmToken, "192.0.2.41"); rec.Code != http.StatusBadRequest {
		t.Fatalf("old confirmation link worked: %d", rec.Code)
	}
	if rec := h.reset(resetToken, resetToPassword, "192.0.2.41"); rec.Code != http.StatusBadRequest {
		t.Fatalf("old reset link worked: %d", rec.Code)
	}
	if h.spy.Passwords["bob"] == resetToPassword {
		t.Fatal("password changed through a dead link")
	}
}

func TestSettingTheSameConfirmedAddressChangesNothing(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	h.confirmedAddress(t, "bob", "bob@example.com")

	if rec := h.setAddress(bob, "BOB@example.com", bobPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if got, _ := h.contacts.Get("bob"); !got.Confirmed || got.Email != "bob@example.com" || len(h.mail.Sent()) != 0 {
		t.Fatalf("a confirmed address must stay as it is: %+v", got)
	}
}

func TestForgotAnswersTheSameForEveryone(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")
	_ = h.contacts.Set("bob", "bob@example.com")

	type answer struct {
		status int
		body   string
	}
	var first *answer
	inputs := map[string]string{
		"confirmed":       "amy",
		"unconfirmed":     "bob",
		"no address":      "cleo",
		"unknown":         "nobody",
		"empty":           "",
		"whitespace":      "   ",
		"uppercase":       "AMY",
		"unicode":         "ämy",
		"over long":       strings.Repeat("a", 500),
		"slashes":         "../amy",
		"newline":         "amy\nbob",
		"another valid":   "cleo",
		"padded":          " amy ",
		"null byte":       "amy\x00",
		"sql ish":         "amy' or '1'='1",
		"mail header ish": "amy@example.com",
	}
	for name, username := range inputs {
		h.portal.email.ipLimit.reset("192.0.2.50")
		h.portal.email.userLimit.reset(username)
		rec := h.forgot(username, "192.0.2.50")
		got := answer{rec.Code, rec.Body.String()}
		if first == nil {
			first = &got
		}
		if got != *first {
			t.Errorf("%s: %+v differs from %+v", name, got, *first)
		}
	}
	if first.status != http.StatusOK || !strings.Contains(strings.ToLower(first.body), genericAnswer) {
		t.Fatalf("unexpected answer %+v", first)
	}
	for _, message := range h.mail.Sent() {
		if message.To != "amy@example.com" {
			t.Fatalf("mail went to %q, only a confirmed address may receive one", message.To)
		}
	}
	if len(h.mail.Sent()) == 0 {
		t.Fatal("the confirmed user got no email")
	}
}

func TestForgotRespondsBeforeDoingTheWork(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")
	var queued []func()
	h.portal.email.background = func(work func()) { queued = append(queued, work) }

	if rec := h.forgot("amy", "192.0.2.50"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if len(h.mail.Sent()) != 0 {
		t.Fatal("the answer must not wait for the mail")
	}
	for _, work := range queued {
		work()
	}
	if len(h.mail.Sent()) != 1 {
		t.Fatalf("got %d mails", len(h.mail.Sent()))
	}
}

func TestForgotEmailsALinkOnTheConfiguredHostNeverTheRequestHost(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")

	req := httptest.NewRequest("POST", "/api/forgot", strings.NewReader(`{"username":"amy"}`))
	req.Host = "evil.example.net"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Host", "evil.example.net")
	req.RemoteAddr = "192.0.2.51:1"
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	sent := h.mail.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, testPortalURL+"/reset#") || strings.Contains(sent[0].Text, "evil") {
		t.Fatalf("unexpected mail %+v", sent)
	}
	if len(sent[0].Subject) > 200 || len(sent[0].Text) > 5000 || strings.ContainsAny(sent[0].Subject, "\r\n") {
		t.Fatal("the message must fit what zero accepts")
	}
	token := h.lastToken(t)
	if strings.Contains(rec.Body.String(), token) || strings.Contains(fmt.Sprint(rec.Header()), token) {
		t.Fatal("the token must never come back to the requester")
	}
}

func TestResetChangesThePasswordOnceAndEndsEverySession(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	first := h.signedIn(t, "bob", bobPassword)
	second := h.signedIn(t, "bob", bobPassword)
	other := h.signedIn(t, "cleo", cleoPassword)
	token := h.requestReset(t, "bob", "192.0.2.60")

	if rec := h.reset(token, resetToPassword, "192.0.2.61"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.spy.Passwords["bob"] != resetToPassword {
		t.Fatal("password not changed")
	}
	for _, cookie := range []*http.Cookie{first, second} {
		if rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie); rec.Code != http.StatusUnauthorized {
			t.Fatalf("an old session survived the reset: %d", rec.Code)
		}
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", other); rec.Code != http.StatusOK {
		t.Fatal("another user's session must survive")
	}
	if h.spy.Passwords["cleo"] == resetToPassword || h.spy.Passwords["amy"] == resetToPassword {
		t.Fatal("someone else's password changed")
	}
	if rec := h.reset(token, "yet another password", "192.0.2.61"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a link must work once, got %d", rec.Code)
	}
}

func TestResetWorksForAdminsToo(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")
	token := h.requestReset(t, "amy", "192.0.2.62")

	if rec := h.reset(token, resetToPassword, "192.0.2.62"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if h.spy.Passwords["amy"] != resetToPassword {
		t.Fatal("the admin's password did not change")
	}
}

func TestResetLinkIsBoundToItsUser(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "same@example.com")
	h.confirmedAddress(t, "bob", "same@example.com")
	amyToken := h.requestReset(t, "amy", "192.0.2.63")
	bobToken := h.requestReset(t, "bob", "192.0.2.64")

	if amyToken == bobToken {
		t.Fatal("tokens must be unique")
	}
	h.reset(amyToken, resetToPassword, "192.0.2.65")
	if h.spy.Passwords["amy"] != resetToPassword || h.spy.Passwords["bob"] == resetToPassword {
		t.Fatal("a link must change only its own user")
	}
	if rec := h.reset(bobToken, "bob's second new password", "192.0.2.65"); rec.Code != http.StatusNoContent {
		t.Fatalf("bob's own link must still work, got %d", rec.Code)
	}
}

func TestResetRefusesBadTokens(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	valid := h.requestReset(t, "bob", "192.0.2.66")

	bad := map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"truncated":   valid[:len(valid)-1],
		"extended":    valid + "A",
		"upper cased": strings.ToUpper(valid),
		"padded":      " " + valid,
		"wrong chars": strings.Repeat("!", 43),
		"all zero":    strings.Repeat("A", 43),
		"huge":        strings.Repeat("A", 100000),
		"username":    "bob",
	}
	for name, token := range bad {
		h.portal.email.tryLimit.reset("192.0.2.67")
		if rec := h.reset(token, resetToPassword, "192.0.2.67"); rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if h.spy.Passwords["bob"] == resetToPassword {
		t.Fatal("a bad token changed the password")
	}
	if rec := h.reset(valid, resetToPassword, "192.0.2.68"); rec.Code != http.StatusNoContent {
		t.Fatalf("the real token must still work after bad guesses, got %d", rec.Code)
	}
}

func TestResetAndConfirmationTokensAreNotInterchangeable(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	_ = h.setAddress(bob, "bob@example.com", bobPassword)
	confirmToken := h.lastToken(t)

	if rec := h.reset(confirmToken, resetToPassword, "192.0.2.70"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a confirmation link reset a password: %d", rec.Code)
	}
	h.confirm(confirmToken, "192.0.2.70")
	resetToken := h.requestReset(t, "bob", "192.0.2.71")
	if rec := h.confirm(resetToken, "192.0.2.70"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a reset link confirmed an address: %d", rec.Code)
	}
}

func TestResetLinkExpiresAfterThirtyMinutes(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.72")

	*h.clock = h.clock.Add(29 * time.Minute)
	if rec := h.reset("x"+token[1:], resetToPassword, "192.0.2.73"); rec.Code != http.StatusBadRequest {
		t.Fatal("a wrong guess must not matter")
	}
	*h.clock = h.clock.Add(2 * time.Minute)
	if rec := h.reset(token, resetToPassword, "192.0.2.73"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an expired link worked: %d", rec.Code)
	}
	if h.spy.Passwords["bob"] == resetToPassword {
		t.Fatal("password changed through an expired link")
	}
}

func TestResetUsesTheSamePasswordRulesAndKeepsTheLinkOnRejection(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.74")

	for name, password := range map[string]string{
		"empty":     "",
		"too short": "short",
		"11 chars":  strings.Repeat("a", 11),
		"too long":  strings.Repeat("a", 129),
		"newline":   "a long password\nwith a newline",
		"nul":       "a long password\x00with a nul",
	} {
		if rec := h.reset(token, password, "192.0.2.75"); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if rec := h.reset(token, bobPassword, "192.0.2.75"); rec.Code != http.StatusNoContent {
		t.Fatalf("a rejected password must not burn the link, and the old password is allowed: got %d", rec.Code)
	}
}

func TestResetFailsWhenTheUserIsGone(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.76")
	if err := h.spy.FakeUsers.Delete("bob"); err != nil {
		t.Fatal(err)
	}

	if rec := h.reset(token, resetToPassword, "192.0.2.77"); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
	if _, found := h.spy.Passwords["bob"]; found {
		t.Fatal("a deleted user came back")
	}
}

func TestOneResetLinkUsedConcurrentlyWorksExactlyOnce(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.78")

	var wins, total int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := h.reset(token, fmt.Sprintf("concurrent password %d", i), fmt.Sprintf("198.51.100.%d", i+1))
			mu.Lock()
			defer mu.Unlock()
			total++
			if rec.Code == http.StatusNoContent {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 || total != 16 {
		t.Fatalf("%d of %d concurrent uses won", wins, total)
	}
}

func TestPasswordChangeByAnotherPathKillsOutstandingResetLinks(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.79")
	bob := h.signedIn(t, "bob", bobPassword)
	body := `{"current":` + quote(bobPassword) + `,"password":"changed by the user itself"}`
	if rec := h.do("PUT", "/api/me/password", body, "192.0.2.10", bob); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}

	if rec := h.reset(token, resetToPassword, "192.0.2.80"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a link issued before a password change must die: %d", rec.Code)
	}
}

func TestForgotIsLimitedPerIPButStillAnswersTheSameBelowTheLimit(t *testing.T) {
	h := newEmailHarness(t)
	for i := range emailIPRequests {
		if rec := h.forgot(fmt.Sprintf("nobody%d", i), "192.0.2.90"); rec.Code != http.StatusOK {
			t.Fatalf("request %d got %d", i, rec.Code)
		}
	}
	if rec := h.forgot("nobody", "192.0.2.90"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.forgot("nobody", "192.0.2.91"); rec.Code != http.StatusOK {
		t.Fatalf("another IP must be unaffected, got %d", rec.Code)
	}
}

func TestForgotIsLimitedPerUserWithoutRevealingIt(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")
	for i := range emailUserSends + 3 {
		if rec := h.forgot("amy", fmt.Sprintf("198.51.100.%d", i+1)); rec.Code != http.StatusOK {
			t.Fatalf("a user limit must not change the answer, got %d", rec.Code)
		}
	}
	if got := len(h.mail.Sent()); got != emailUserSends {
		t.Fatalf("got %d mails, want %d", got, emailUserSends)
	}
}

func TestMailSendsAreCappedOverall(t *testing.T) {
	h := newEmailHarness(t)
	for i := range emailTotalSends + 5 {
		name := fmt.Sprintf("user%02d", i)
		h.spy.Passwords[name] = ""
		h.confirmedAddress(t, name, name+"@example.com")
		h.forgot(name, fmt.Sprintf("203.0.113.%d", i+1))
	}
	if got := len(h.mail.Sent()); got != emailTotalSends {
		t.Fatalf("got %d mails, want %d", got, emailTotalSends)
	}
}

func TestResetAttemptsAreLimitedPerIP(t *testing.T) {
	h := newEmailHarness(t)
	for range emailTryRequests {
		h.reset(strings.Repeat("A", 43), resetToPassword, "192.0.2.92")
	}
	if rec := h.reset(strings.Repeat("A", 43), resetToPassword, "192.0.2.92"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.confirm(strings.Repeat("A", 43), "192.0.2.92"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("confirming shares the limit, got %d", rec.Code)
	}
}

func TestMailFailureStillGivesTheGenericAnswerAndLogsNothingSecret(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	for name, failure := range map[string]error{
		"unauthorized": mail.ErrUnauthorized,
		"rejected":     mail.ErrRejected,
		"rate limited": mail.ErrRateLimited,
		"unavailable":  mail.ErrUnavailable,
	} {
		h := newEmailHarness(t)
		h.confirmedAddress(t, "amy", "secret-address@example.com")
		good := h.forgot("amy", "192.0.2.100")
		h.portal.email.userLimit.reset("amy")
		h.mail.Err = failure
		bad := h.forgot("amy", "192.0.2.101")
		if bad.Code != good.Code || bad.Body.String() != good.Body.String() {
			t.Errorf("%s: the answer changed when mail failed", name)
		}
	}
	if logged.Len() == 0 {
		t.Fatal("a failed send must be logged")
	}
	for _, secret := range []string{"secret-address@example.com", "/reset#", "Bearer"} {
		if strings.Contains(logged.String(), secret) {
			t.Fatalf("the log leaks %q: %s", secret, logged.String())
		}
	}
}

func TestSettingAnAddressReportsAMailFailure(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	h.mail.Err = mail.ErrUnavailable

	if rec := h.setAddress(bob, "bob@example.com", bobPassword); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSettingAnAddressIsLimitedPerUser(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	for i := range emailUserSends {
		if rec := h.setAddress(bob, fmt.Sprintf("bob%d@example.com", i), bobPassword); rec.Code != http.StatusNoContent {
			t.Fatalf("got %d", rec.Code)
		}
	}
	if rec := h.setAddress(bob, "bobx@example.com", bobPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestTokensAreKeptHashedOnly(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.requestReset(t, "bob", "192.0.2.110")

	if dump := fmt.Sprintf("%v %+v %#v", h.portal.email.tokens.items, h.portal.email.tokens.items, h.portal.email.tokens.items); strings.Contains(dump, token) {
		t.Fatal("the raw token is held in memory")
	}
	files, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(h.dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), token) {
			t.Fatalf("the raw token reached the disk in %s", file.Name())
		}
	}
}

func TestAddressesAreNotServedAnywhere(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	amy := h.signedIn(t, "amy", amyPassword)

	for _, path := range []string{"/contacts.json", "/static/contacts.json", "/static/../contacts.json", "/static/%2e%2e/contacts.json", "/roles.json"} {
		rec := h.do("GET", path, "", "192.0.2.111", amy)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "bob@example.com") {
			t.Errorf("%s: got %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/me", "/api/me/email", "/api/admin/users"} {
		if body := h.do("GET", path, "", "192.0.2.111", amy).Body.String(); strings.Contains(body, "bob@example.com") {
			t.Errorf("%s shows another user's address", path)
		}
	}
}

func TestDeletingAUserForgetsTheirAddress(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	amy := h.signedIn(t, "amy", amyPassword)

	body := `{"confirm":` + quote(amyPassword) + `}`
	if rec := h.do("DELETE", "/api/admin/users/bob", body, "192.0.2.112", amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if _, found := h.contacts.Get("bob"); found {
		t.Fatal("a deleted user's address was kept, so a new user with that name would inherit it")
	}
}

func TestAnOwnerDeletingAUserForgetsTheirAddress(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	if err := h.spy.FakeUsers.Delete("bob"); err != nil {
		t.Fatal(err)
	}

	h.portal.EndSessions("bob")
	if _, found := h.contacts.Get("bob"); found {
		t.Fatal("the address stayed after the account was removed")
	}
	h.confirmedAddress(t, "cleo", "cleo@example.com")
	h.portal.EndSessions("cleo")
	if _, found := h.contacts.Get("cleo"); !found {
		t.Fatal("an owner password change must not drop the address")
	}
}

func TestEmailResetIsOffWithoutMailConfig(t *testing.T) {
	h := newPortalHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	rec := h.do("GET", "/api/forgot", "", "192.0.2.120", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("POST", "/api/forgot", `{"username":"amy"}`, "192.0.2.120", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	put := h.do("PUT", "/api/me/email", `{"email":"amy@example.com","current":`+quote(amyPassword)+`}`, "192.0.2.10", amy)
	if put.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", put.Code)
	}
	if rec := h.do("POST", "/api/reset", `{"token":"`+strings.Repeat("A", 43)+`","password":"a brand new password"}`, "192.0.2.120", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
	if !strings.Contains(h.do("GET", "/api/me/email", "", "192.0.2.10", amy).Body.String(), `"enabled":false`) {
		t.Fatal("the portal must be told the feature is off")
	}
}

func TestEmailPagesAreServed(t *testing.T) {
	h := newEmailHarness(t)
	for _, path := range []string{"/forgot", "/reset", "/confirm-email"} {
		rec := h.do("GET", path, "", "192.0.2.130", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: got %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := h.do("GET", "/reset/extra", "", "192.0.2.130", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestEmailEndpointsRefuseCrossOriginAndWrongMethods(t *testing.T) {
	h := newEmailHarness(t)
	req := httptest.NewRequest("POST", "/api/forgot", strings.NewReader(`{"username":"amy"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.net")
	req.RemoteAddr = "192.0.2.131:1"
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	for _, path := range []string{"/api/forgot", "/api/reset", "/api/email/confirm"} {
		if rec := h.do("PUT", path, `{}`, "192.0.2.131", nil); rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
			t.Errorf("PUT %s got %d", path, rec.Code)
		}
	}
	if rec := h.do("POST", "/api/forgot", `{"username":"amy"}`, "192.0.2.132", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	plain := httptest.NewRequest("POST", "/api/forgot", strings.NewReader(`{"username":"amy"}`))
	plain.RemoteAddr = "192.0.2.133:1"
	plainRec := httptest.NewRecorder()
	h.portal.ServeHTTP(plainRec, plain)
	if plainRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", plainRec.Code)
	}
}
