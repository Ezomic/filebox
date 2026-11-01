package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

func (h *emailHarness) loginAs(identifier, password, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/login", `{"username":`+quote(identifier)+`,"password":`+quote(password)+`}`, ip, nil)
}

func (h *emailHarness) requestLink(address, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/sign-in-link", `{"email":`+quote(address)+`}`, ip, nil)
}

func (h *emailHarness) useLink(token, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/sign-in", `{"token":`+quote(token)+`}`, ip, nil)
}

func (h *emailHarness) mailedLink(t *testing.T, address, ip string) string {
	t.Helper()
	before := len(h.mail.Sent())
	if rec := h.requestLink(address, ip); rec.Code != http.StatusOK {
		t.Fatalf("request got %d %s", rec.Code, rec.Body)
	}
	if len(h.mail.Sent()) != before+1 {
		t.Fatalf("expected one new email, have %d", len(h.mail.Sent())-before)
	}

	return h.lastToken(t)
}

func portalSession(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == portalCookieName {
			return cookie
		}
	}

	return nil
}

func (h *emailHarness) whoAmI(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("not signed in: %d", rec.Code)
	}

	return rec.Body.String()
}

func TestSignInWithAConfirmedEmailAddressAndPassword(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "Bob.Smith@Example.com")

	for _, identifier := range []string{"Bob.Smith@Example.com", "bob.smith@example.com", "BOB.SMITH@EXAMPLE.COM", "  bob.smith@example.com \t"} {
		rec := h.loginAs(identifier, bobPassword, "192.0.2.10")
		cookie := portalSession(rec)
		if rec.Code != http.StatusNoContent || cookie == nil {
			t.Fatalf("%q: got %d %s", identifier, rec.Code, rec.Body)
		}
		if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%q: weak cookie %+v", identifier, cookie)
		}
		if body := h.whoAmI(t, cookie); !strings.Contains(body, `"bob"`) {
			t.Fatalf("%q: session belongs to %s", identifier, body)
		}
	}
	if rec := h.loginAs("bob", bobPassword, "192.0.2.10"); rec.Code != http.StatusNoContent {
		t.Fatalf("the plain username must keep working, got %d", rec.Code)
	}
}

func TestAdminsMayUseAnEmailAddressWithTheirPassword(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")

	rec := h.loginAs("amy@example.com", amyPassword, "192.0.2.10")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if body := h.whoAmI(t, portalSession(rec)); !strings.Contains(body, `"admin":true`) {
		t.Fatalf("got %s", body)
	}
}

func TestEmailSignInFailuresLookLikeAWrongUsername(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	h.confirmedAddress(t, "amy", "amy@example.com")
	_ = h.contacts.Set("cleo", "cleo@example.com")

	reference := h.loginAs("nobody", bobPassword, "192.0.2.10")
	if reference.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", reference.Code)
	}
	cases := map[string][2]string{
		"wrong password":                  {"bob@example.com", "not the password"},
		"unknown address":                 {"nobody@example.com", bobPassword},
		"unconfirmed address":             {"cleo@example.com", cleoPassword},
		"another user's address":          {"amy@example.com", bobPassword},
		"another user's name":             {"cleo", bobPassword},
		"empty":                           {"", ""},
		"whitespace":                      {"   ", bobPassword},
		"full width at":                   {"bob＠example.com", bobPassword},
		"display name":                    {"Bob <bob@example.com>", bobPassword},
		"trailing dot":                    {"bob@example.com.", bobPassword},
		"newline":                         {"bob@example.com\nBcc: evil@example.com", bobPassword},
		"unicode":                         {"bøb@example.com", bobPassword},
		"idn":                             {"bob@bücher.example", bobPassword},
		"over long":                       {strings.Repeat("a", 300) + "@example.com", bobPassword},
		"username shaped like an address": {"bob@bob", bobPassword},
		"two ats":                         {"bob@example.com@example.com", bobPassword},
		"comma list":                      {"bob@example.com,amy@example.com", bobPassword},
	}
	for name, creds := range cases {
		h.portal.ipLimit.reset("192.0.2.11")
		rec := h.loginAs(creds[0], creds[1], "192.0.2.11")
		if rec.Code != reference.Code || rec.Body.String() != reference.Body.String() {
			t.Errorf("%s: %d %q differs from %d %q", name, rec.Code, rec.Body, reference.Code, reference.Body)
		}
		if portalSession(rec) != nil {
			t.Errorf("%s: a session was started", name)
		}
	}
}

func TestEmailSignInDoesTheSamePasswordWorkForEveryIdentifier(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	_ = h.contacts.Set("cleo", "cleo@example.com")

	for name, identifier := range map[string]string{
		"known address":     "bob@example.com",
		"unknown address":   "nobody@example.com",
		"unconfirmed":       "cleo@example.com",
		"unknown username":  "nobody",
		"existing username": "bob",
	} {
		h.portal.ipLimit.reset("192.0.2.12")
		before := h.verifier.Calls
		h.loginAs(identifier, "a wrong password here", "192.0.2.12")
		if got := h.verifier.Calls - before; got != 1 {
			t.Errorf("%s: %d password checks, want 1", name, got)
		}
	}
}

func TestAnUnresolvedAddressNeverSignsInEvenIfTheDecoyAccountExists(t *testing.T) {
	h := newEmailHarness(t)
	h.verifier.Passwords[signInDecoy] = "the decoy password"

	rec := h.loginAs("nobody@example.com", "the decoy password", "192.0.2.13")
	if rec.Code != http.StatusUnauthorized || portalSession(rec) != nil {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAnAddressConfirmedOnOneAccountCannotSignInAsAnother(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")

	if rec := h.loginAs("bob@example.com", cleoPassword, "192.0.2.14"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.loginAs("bob@example.com", amyPassword, "192.0.2.14"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestOnlyOneAccountCanConfirmAnAddressAndTheRefusalComesWhenTheLinkIsUsed(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	cleo := h.signedIn(t, "cleo", cleoPassword)

	first := h.setAddress(bob, "Shared@Example.com", bobPassword)
	bobToken := h.lastToken(t)
	second := h.setAddress(cleo, "shared@example.com", cleoPassword)
	cleoToken := h.lastToken(t)
	if first.Code != second.Code || first.Body.String() != second.Body.String() || len(h.mail.Sent()) != 2 {
		t.Fatalf("saving a taken address must look the same: %d %q vs %d %q", first.Code, first.Body, second.Code, second.Body)
	}

	if rec := h.confirm(bobToken, "192.0.2.15"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.confirm(cleoToken, "192.0.2.15"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the second account must be refused, got %d", rec.Code)
	}
	if got, _ := h.contacts.Get("cleo"); got.Confirmed {
		t.Fatal("cleo confirmed an address that bob holds")
	}
	if rec := h.loginAs("shared@example.com", cleoPassword, "192.0.2.15"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unconfirmed duplicate must not sign in, got %d", rec.Code)
	}
	if rec := h.loginAs("shared@example.com", bobPassword, "192.0.2.15"); rec.Code != http.StatusNoContent {
		t.Fatalf("the owner must sign in, got %d", rec.Code)
	}
}

func TestTwoAccountsRacingThroughTheirLinksConfirmOneAddress(t *testing.T) {
	h := newEmailHarness(t)
	bob := h.signedIn(t, "bob", bobPassword)
	cleo := h.signedIn(t, "cleo", cleoPassword)
	_ = h.setAddress(bob, "race@example.com", bobPassword)
	bobToken := h.lastToken(t)
	_ = h.setAddress(cleo, "race@example.com", cleoPassword)
	cleoToken := h.lastToken(t)

	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for i, token := range []string{bobToken, cleoToken} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := h.confirm(token, fmt.Sprintf("198.51.100.%d", i+1))
			mu.Lock()
			defer mu.Unlock()
			if rec.Code == http.StatusNoContent {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d accounts confirmed one address", wins)
	}
}

func TestEmailSignInIsLimitedPerIPAndPerAccountAcrossBothSpellings(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")

	for i := range portalIPFails {
		if rec := h.loginAs("bob@example.com", "wrong password number "+fmt.Sprint(i), "192.0.2.16"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d", i, rec.Code)
		}
	}
	if rec := h.loginAs("bob@example.com", bobPassword, "192.0.2.16"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the IP must be blocked, got %d", rec.Code)
	}

	for i := range portalUserFails - portalIPFails {
		identifier := "bob"
		if i%2 == 0 {
			identifier = "bob@example.com"
		}
		if rec := h.loginAs(identifier, "wrong password again", fmt.Sprintf("198.51.100.%d", i+1)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d", i, rec.Code)
		}
	}
	for _, identifier := range []string{"bob", "bob@example.com", "BOB@example.com"} {
		if rec := h.loginAs(identifier, bobPassword, "203.0.113.1"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: switching spelling must not bypass the account limit, got %d", identifier, rec.Code)
		}
	}
}

func TestUnknownAddressesAreAlsoLimitedPerIP(t *testing.T) {
	h := newEmailHarness(t)
	for i := range portalIPFails {
		h.loginAs(fmt.Sprintf("nobody%d@example.com", i), bobPassword, "192.0.2.17")
	}
	if rec := h.loginAs("nobody@example.com", bobPassword, "192.0.2.17"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestEmailSignInStillWorksWhenMailIsNotConfigured(t *testing.T) {
	dir := t.TempDir()
	contacts, err := auth.OpenContacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := auth.Open(dir)
	verifier := &system.FakeVerifier{Passwords: map[string]string{"bob": bobPassword}}
	spy := &spyUsers{FakeUsers: system.NewFakeUsers("bob")}
	portal := NewPortal(PortalDeps{Verifier: verifier, Users: spy, Roles: roles, Contacts: contacts})
	h := &portalHarness{portal: portal, verifier: verifier, roles: roles, spy: spy}
	_ = contacts.Set("bob", "bob@example.com")
	_, _ = contacts.Confirm("bob", "bob@example.com")

	if rec := h.login("bob@example.com", bobPassword, "192.0.2.18"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("POST", "/api/sign-in-link", `{"email":"bob@example.com"}`, "192.0.2.18", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("POST", "/api/sign-in", `{"token":"`+strings.Repeat("A", 43)+`"}`, "192.0.2.18", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
	if !strings.Contains(h.do("GET", "/api/forgot", "", "192.0.2.18", nil).Body.String(), `"enabled":false`) {
		t.Fatal("the page must be told the link option is off")
	}
}

func TestLinkRequestAnswersTheSameForEveryAddress(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	h.confirmedAddress(t, "amy", "amy@example.com")
	_ = h.contacts.Set("cleo", "cleo@example.com")

	type answer struct {
		status int
		body   string
	}
	var first *answer
	inputs := map[string]string{
		"confirmed":        "bob@example.com",
		"uppercase":        "BOB@EXAMPLE.COM",
		"padded":           "  bob@example.com ",
		"admin":            "amy@example.com",
		"unconfirmed":      "cleo@example.com",
		"unknown":          "nobody@example.com",
		"empty":            "",
		"whitespace":       "   ",
		"not an address":   "bob",
		"full width at":    "bob＠example.com",
		"display name":     "Bob <bob@example.com>",
		"newline":          "bob@example.com\nBcc: evil@example.com",
		"trailing dot":     "bob@example.com.",
		"over long":        strings.Repeat("a", 500) + "@example.com",
		"a username":       "cleo",
		"two addresses":    "bob@example.com,amy@example.com",
		"unicode":          "bøb@example.com",
		"tab inside":       "bob\t@example.com",
		"null byte":        "bob@example.com\x00",
		"sql ish":          "bob@example.com' or '1'='1",
		"another valid":    "other@example.com",
		"unconfirmed twin": "CLEO@example.com",
	}
	for name, address := range inputs {
		h.portal.email.ipLimit.reset("link:192.0.2.20")
		h.portal.email.userLimit.reset("signin:bob")
		rec := h.requestLink(address, "192.0.2.20")
		got := answer{rec.Code, rec.Body.String()}
		if first == nil {
			first = &got
		}
		if got != *first {
			t.Errorf("%s: %+v differs from %+v", name, got, *first)
		}
	}
	if first.status != http.StatusOK || !strings.Contains(strings.ToLower(first.body), "if that address belongs to an account, we sent a link") {
		t.Fatalf("unexpected answer %+v", first)
	}
	for _, message := range h.mail.Sent() {
		if message.To != "bob@example.com" {
			t.Fatalf("mail went to %q, only a confirmed non-admin address may receive a link", message.To)
		}
	}
	if len(h.mail.Sent()) == 0 {
		t.Fatal("the confirmed user got no email")
	}
}

func TestLinkRequestRespondsBeforeDoingTheWork(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	var queued []func()
	h.portal.email.background = func(work func()) { queued = append(queued, work) }

	if rec := h.requestLink("bob@example.com", "192.0.2.21"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if len(h.mail.Sent()) != 0 {
		t.Fatal("the answer must not wait for the lookup or the mail")
	}
	for _, work := range queued {
		work()
	}
	if len(h.mail.Sent()) != 1 {
		t.Fatalf("got %d mails", len(h.mail.Sent()))
	}
}

func TestLinkEmailPointsAtTheConfiguredHostAndNeverComesBack(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "Bob@Example.com")
	logged := captureLog(t)

	req := httptest.NewRequest("POST", "/api/sign-in-link", strings.NewReader(`{"email":"bob@example.com"}`))
	req.Host = "evil.example.net"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Host", "evil.example.net")
	req.RemoteAddr = "192.0.2.22:1"
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	sent := h.mail.Sent()
	if len(sent) != 1 || sent[0].To != "Bob@Example.com" || !strings.Contains(sent[0].Text, testPortalURL+"/sign-in#") || strings.Contains(sent[0].Text, "evil") {
		t.Fatalf("unexpected mail %+v", sent)
	}
	if strings.Contains(sent[0].Text, "?") || !strings.Contains(sent[0].Text, "15 minutes") {
		t.Fatalf("the link must use the fragment, not a query, and state its lifetime: %q", sent[0].Text)
	}
	if len(sent[0].Subject) > 200 || len(sent[0].Text) > 5000 || strings.ContainsAny(sent[0].Subject, "\r\n") {
		t.Fatal("the message must fit what zero accepts")
	}
	token := h.lastToken(t)
	if strings.Contains(rec.Body.String(), token) || strings.Contains(fmt.Sprint(rec.Header()), token) {
		t.Fatal("the token came back to the requester")
	}
	if strings.Contains(logged.String(), token) {
		t.Fatal("the token was written to the log")
	}
}

func TestTheLinkSignsInOnceAndOnlyAsItsAccount(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	h.confirmedAddress(t, "cleo", "cleo@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.23")
	logged := captureLog(t)

	body := `{"token":` + quote(token) + `,"username":"cleo","email":"cleo@example.com"}`
	rec := h.do("POST", "/api/sign-in", body, "192.0.2.24", nil)
	cookie := portalSession(rec)
	if rec.Code != http.StatusNoContent || cookie == nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("weak cookie %+v", cookie)
	}
	if me := h.whoAmI(t, cookie); !strings.Contains(me, `"bob"`) || strings.Contains(me, "cleo") {
		t.Fatalf("the session must be the account that asked: %s", me)
	}
	if strings.Contains(logged.String(), token) {
		t.Fatal("the token was written to the log")
	}
	if h.spy.Passwords["bob"] != "" {
		t.Fatal("a link sign-in must not change the password")
	}

	again := h.useLink(token, "192.0.2.25")
	if again.Code != http.StatusBadRequest || portalSession(again) != nil {
		t.Fatalf("a link must work once, got %d", again.Code)
	}
	if h.verifier.Passwords["bob"] != bobPassword {
		t.Fatal("the password changed")
	}
}

func TestOpeningTheLinkPageDoesNotUseTheToken(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.26")

	for _, target := range []string{"/sign-in", "/sign-in?token=" + token, "/sign-in/" + token, "/api/sign-in?token=" + token} {
		rec := h.do("GET", target, "", "192.0.2.27", nil)
		if portalSession(rec) != nil || strings.Contains(rec.Body.String(), token) {
			t.Fatalf("%s started a session or echoed the token", target)
		}
	}
	if page := h.do("GET", "/sign-in", "", "192.0.2.27", nil); page.Code != http.StatusOK || !strings.Contains(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("got %d", page.Code)
	}
	if rec := h.useLink(token, "192.0.2.28"); rec.Code != http.StatusNoContent {
		t.Fatalf("a scanner that only GETs the page burnt the link: %d", rec.Code)
	}
}

func TestTheLinkExpiresAfterFifteenMinutes(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.29")

	*h.clock = h.clock.Add(15*time.Minute + time.Second)
	if rec := h.useLink(token, "192.0.2.30"); rec.Code != http.StatusBadRequest || portalSession(rec) != nil {
		t.Fatalf("got %d", rec.Code)
	}
	if signInLinkTTL != 15*time.Minute {
		t.Fatalf("got %s", signInLinkTTL)
	}
}

func TestTheLinkStillWorksJustBeforeItExpires(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.31")

	*h.clock = h.clock.Add(15*time.Minute - time.Second)
	if rec := h.useLink(token, "192.0.2.32"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAdminsCannotUseTheLink(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "amy", "amy@example.com")

	if rec := h.requestLink("amy@example.com", "192.0.2.33"); rec.Code != http.StatusOK || len(h.mail.Sent()) != 0 {
		t.Fatalf("no link may be mailed to an admin: %d, %d mails", rec.Code, len(h.mail.Sent()))
	}
}

func TestALinkIsRefusedOnceTheAccountBecameAnAdmin(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.34")
	if err := h.roles.Promote("bob"); err != nil {
		t.Fatal(err)
	}

	rec := h.useLink(token, "192.0.2.35")
	if rec.Code != http.StatusBadRequest || portalSession(rec) != nil || !strings.Contains(rec.Body.String(), linkInvalid) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestALinkDiesWithAPasswordChangeOrReset(t *testing.T) {
	t.Run("change by the user", func(t *testing.T) {
		h := newEmailHarness(t)
		h.confirmedAddress(t, "bob", "bob@example.com")
		token := h.mailedLink(t, "bob@example.com", "192.0.2.36")
		bob := h.signedIn(t, "bob", bobPassword)
		body := `{"current":` + quote(bobPassword) + `,"password":"changed by the user itself"}`
		if rec := h.do("PUT", "/api/me/password", body, "192.0.2.10", bob); rec.Code != http.StatusNoContent {
			t.Fatalf("got %d", rec.Code)
		}
		if rec := h.useLink(token, "192.0.2.37"); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("reset by email", func(t *testing.T) {
		h := newEmailHarness(t)
		h.confirmedAddress(t, "bob", "bob@example.com")
		link := h.mailedLink(t, "bob@example.com", "192.0.2.38")
		reset := h.requestReset(t, "bob", "192.0.2.39")
		if rec := h.reset(reset, resetToPassword, "192.0.2.40"); rec.Code != http.StatusNoContent {
			t.Fatalf("got %d", rec.Code)
		}
		if rec := h.useLink(link, "192.0.2.41"); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("change by the owner", func(t *testing.T) {
		h := newEmailHarness(t)
		h.confirmedAddress(t, "bob", "bob@example.com")
		token := h.mailedLink(t, "bob@example.com", "192.0.2.42")
		h.portal.EndSessions("bob")
		if rec := h.useLink(token, "192.0.2.43"); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

func TestALinkDiesWhenTheAddressChangesOrTheAccountIsDeleted(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	h.confirmedAddress(t, "cleo", "cleo@example.com")
	changed := h.mailedLink(t, "bob@example.com", "192.0.2.44")
	gone := h.mailedLink(t, "cleo@example.com", "192.0.2.45")

	bob := h.signedIn(t, "bob", bobPassword)
	_ = h.setAddress(bob, "new@example.com", bobPassword)
	if err := h.spy.FakeUsers.Delete("cleo"); err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"changed address": changed, "deleted account": gone} {
		if rec := h.useLink(token, "192.0.2.46"); rec.Code != http.StatusBadRequest || portalSession(rec) != nil {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
}

func TestALinkIsRefusedWhenItsAddressNowBelongsToAnotherAccount(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.47")
	if err := h.contacts.Forget("bob"); err != nil {
		t.Fatal(err)
	}
	h.confirmedAddress(t, "cleo", "bob@example.com")

	if rec := h.useLink(token, "192.0.2.48"); rec.Code != http.StatusBadRequest || portalSession(rec) != nil {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUsingALinkInvalidatesOutstandingSignInAndResetLinksOnly(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	first := h.mailedLink(t, "bob@example.com", "192.0.2.49")
	second := h.mailedLink(t, "bob@example.com", "192.0.2.50")
	reset := h.requestReset(t, "bob", "192.0.2.51")
	confirm, err := h.portal.email.tokens.issue(kindConfirm, "bob", "bob@example.com", 0, confirmLinkTTL)
	if err != nil {
		t.Fatal(err)
	}

	if rec := h.useLink(first, "192.0.2.52"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.useLink(second, "192.0.2.53"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the other sign-in link must be dead, got %d", rec.Code)
	}
	if rec := h.reset(reset, resetToPassword, "192.0.2.54"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the reset link must be dead, got %d", rec.Code)
	}
	if rec := h.confirm(confirm, "192.0.2.55"); rec.Code != http.StatusNoContent {
		t.Fatalf("a pending confirmation is not a sign-in path and must survive, got %d", rec.Code)
	}
}

func TestLinkRefusesBadTokens(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	real := h.mailedLink(t, "bob@example.com", "192.0.2.56")

	bad := map[string]string{
		"empty":       "",
		"short":       real[:20],
		"truncated":   real[:42],
		"too long":    real + "A",
		"padded":      " " + real,
		"newline":     real + "\n",
		"wrong chars": strings.Repeat("!", 43),
		"unknown":     strings.Repeat("A", 43),
		"upper":       strings.ToUpper(real),
	}
	for name, token := range bad {
		h.portal.email.tryLimit.reset("192.0.2.57")
		rec := h.useLink(token, "192.0.2.57")
		if rec.Code != http.StatusBadRequest || portalSession(rec) != nil || !strings.Contains(rec.Body.String(), linkInvalid) {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := h.do("POST", "/api/sign-in", `{"token":1}`, "192.0.2.57", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("non-string token got %d", rec.Code)
	}
	if rec := h.do("POST", "/api/sign-in", `{}`, "192.0.2.57", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("missing token got %d", rec.Code)
	}
	if rec := h.useLink(real, "192.0.2.58"); rec.Code != http.StatusNoContent {
		t.Fatalf("a bad guess must not burn the real link, got %d", rec.Code)
	}
}

func TestLinkKindsAreNotInterchangeable(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	reset := h.requestReset(t, "bob", "192.0.2.59")
	signIn := h.mailedLink(t, "bob@example.com", "192.0.2.60")
	bob := h.signedIn(t, "bob", bobPassword)
	_ = h.setAddress(bob, "other@example.com", bobPassword)
	confirm := h.lastToken(t)

	for name, token := range map[string]string{"reset": reset, "confirm": confirm} {
		if rec := h.useLink(token, "192.0.2.61"); rec.Code != http.StatusBadRequest || portalSession(rec) != nil {
			t.Errorf("a %s link signed in: %d", name, rec.Code)
		}
	}
	if rec := h.reset(signIn, resetToPassword, "192.0.2.62"); rec.Code != http.StatusBadRequest || h.spy.Passwords["bob"] == resetToPassword {
		t.Fatalf("a sign-in link reset a password: %d", rec.Code)
	}
	if rec := h.confirm(signIn, "192.0.2.63"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a sign-in link confirmed an address: %d", rec.Code)
	}
}

func TestOneSignInLinkUsedConcurrentlyWorksExactlyOnce(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.65")

	var mu sync.Mutex
	wins, total := 0, 0
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := h.useLink(token, fmt.Sprintf("198.51.100.%d", i+1))
			mu.Lock()
			defer mu.Unlock()
			total++
			if rec.Code == http.StatusNoContent && portalSession(rec) != nil {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 || total != 16 {
		t.Fatalf("%d of %d concurrent uses won", wins, total)
	}
}

func TestRepeatedLinkRequestsAreCappedPerAccountIPAndOverall(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")

	for i := range emailUserSends + 3 {
		if rec := h.requestLink("bob@example.com", fmt.Sprintf("198.51.100.%d", i+1)); rec.Code != http.StatusOK {
			t.Fatalf("a cap must not change the answer, got %d", rec.Code)
		}
	}
	if got := len(h.mail.Sent()); got != emailUserSends {
		t.Fatalf("got %d mails for one account, want %d", got, emailUserSends)
	}

	for i := range emailIPRequests {
		if rec := h.requestLink("nobody@example.com", "192.0.2.66"); rec.Code != http.StatusOK {
			t.Fatalf("request %d got %d", i, rec.Code)
		}
	}
	if rec := h.requestLink("nobody@example.com", "192.0.2.66"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.requestLink("nobody@example.com", "192.0.2.67"); rec.Code != http.StatusOK {
		t.Fatalf("another IP must be unaffected, got %d", rec.Code)
	}
}

func TestLinkMailSharesTheOverallHourlyCapBelowZerosLimit(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	if emailTotalSends >= 20 {
		t.Fatalf("the shared cap %d must stay below zero's 20 sends an hour", emailTotalSends)
	}
	for range emailTotalSends {
		h.portal.email.totalLimit.reserve("all")
	}

	if rec := h.requestLink("bob@example.com", "192.0.2.68"); rec.Code != http.StatusOK || len(h.mail.Sent()) != 0 {
		t.Fatalf("the cap must hold silently: %d, %d mails", rec.Code, len(h.mail.Sent()))
	}
}

func TestLinkUseAttemptsAreLimitedPerIP(t *testing.T) {
	h := newEmailHarness(t)
	for i := range emailTryRequests {
		if rec := h.useLink(strings.Repeat("A", 43), "192.0.2.69"); rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d got %d", i, rec.Code)
		}
	}
	if rec := h.useLink(strings.Repeat("A", 43), "192.0.2.69"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestMailFailureOnALinkRequestStillGivesTheGenericAnswer(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	h.mail.Err = fmt.Errorf("zero is down")
	logged := captureLog(t)

	rec := h.requestLink("bob@example.com", "192.0.2.70")
	if rec.Code != http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "we sent a link") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(logged.String(), "bob@example.com") {
		t.Fatalf("the address was logged: %s", logged)
	}
}

func TestSignInEndpointsRefuseCrossOriginAndWrongMethods(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.71")

	req := httptest.NewRequest("POST", "/api/sign-in", strings.NewReader(`{"token":`+quote(token)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.net")
	req.RemoteAddr = "192.0.2.72:1"
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	for _, path := range []string{"/api/sign-in", "/api/sign-in-link"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			if rec := h.do(method, path, `{}`, "192.0.2.73", nil); rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
				t.Errorf("%s %s got %d", method, path, rec.Code)
			}
		}
	}
	plain := httptest.NewRequest("POST", "/api/sign-in", strings.NewReader(`{"token":`+quote(token)+`}`))
	plain.RemoteAddr = "192.0.2.74:1"
	plainRec := httptest.NewRecorder()
	h.portal.ServeHTTP(plainRec, plain)
	if plainRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", plainRec.Code)
	}
	if rec := h.useLink(token, "192.0.2.75"); rec.Code != http.StatusNoContent {
		t.Fatalf("the refused requests must not burn the link, got %d", rec.Code)
	}
}

func TestSignInPagesAreServed(t *testing.T) {
	h := newEmailHarness(t)
	for _, path := range []string{"/sign-in", "/sign-in-link"} {
		rec := h.do("GET", path, "", "192.0.2.76", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: got %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestSignInTokensAreKeptHashedOnly(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	token := h.mailedLink(t, "bob@example.com", "192.0.2.77")

	if dump := fmt.Sprintf("%v %+v %#v", h.portal.email.tokens.items, h.portal.email.tokens.items, h.portal.email.tokens.items); strings.Contains(dump, token) {
		t.Fatal("the raw token is held in memory")
	}
}
