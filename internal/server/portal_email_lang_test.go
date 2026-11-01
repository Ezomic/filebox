package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/mail"
)

type mailKind struct {
	name    string
	path    string
	fresh   bool
	trigger func(h *emailHarness, t *testing.T, lang, ip string) *httptest.ResponseRecorder
	english mail.Message
	dutch   mail.Message
}

func withLang(fields, lang string) string {
	if lang == "" {
		return "{" + fields + "}"
	}

	return "{" + fields + `,"lang":` + lang + "}"
}

func mailKinds() []mailKind {
	return []mailKind{
		{
			name: "reset",
			path: "/reset#",
			trigger: func(h *emailHarness, _ *testing.T, lang, ip string) *httptest.ResponseRecorder {
				return h.do("POST", "/api/forgot", withLang(`"username":"bob"`, lang), ip, nil)
			},
			english: mail.Message{
				To:      "bob@example.com",
				Subject: "Reset your Filebox password",
				Text: "Hello,\n\nSomeone asked to reset the password of the Filebox account \"bob\". Open this link within 30 minutes to choose a new password:\n\n" +
					testPortalURL + "/reset#TOKEN\n\nThe link works once. If you did not ask for this, ignore this email and your password stays as it is.\n",
			},
			dutch: mail.Message{
				To:      "bob@example.com",
				Subject: "Wachtwoord opnieuw instellen voor Filebox",
				Text: "Hallo,\n\nIemand heeft gevraagd om het wachtwoord van het account \"bob\" op Filebox opnieuw in te stellen. Open binnen 30 minuten deze link om een nieuw wachtwoord te kiezen:\n\n" +
					testPortalURL + "/reset#TOKEN\n\nDe link werkt maar een keer. Heb je dit niet aangevraagd, negeer deze e-mail dan. Je wachtwoord blijft zoals het is.\n",
			},
		},
		{
			name: "sign-in",
			path: "/sign-in#",
			trigger: func(h *emailHarness, _ *testing.T, lang, ip string) *httptest.ResponseRecorder {
				return h.do("POST", "/api/sign-in-link", withLang(`"email":"bob@example.com"`, lang), ip, nil)
			},
			english: mail.Message{
				To:      "bob@example.com",
				Subject: "Your Filebox sign-in link",
				Text: "Hello,\n\nSomeone asked for a link to sign in to the Filebox account \"bob\". Open this link within 15 minutes and press the button to sign in:\n\n" +
					testPortalURL + "/sign-in#TOKEN\n\nThe link works once and does not change your password. If you did not ask for this, ignore this email.\n",
			},
			dutch: mail.Message{
				To:      "bob@example.com",
				Subject: "Je inloglink voor Filebox",
				Text: "Hallo,\n\nIemand heeft gevraagd om een link om in te loggen op het account \"bob\" op Filebox. Open binnen 15 minuten deze link en druk op de knop om in te loggen:\n\n" +
					testPortalURL + "/sign-in#TOKEN\n\nDe link werkt maar een keer en verandert je wachtwoord niet. Heb je dit niet aangevraagd, negeer deze e-mail dan.\n",
			},
		},
		{
			name:  "confirmation",
			path:  "/confirm-email#",
			fresh: true,
			trigger: func(h *emailHarness, t *testing.T, lang, ip string) *httptest.ResponseRecorder {
				bob := h.signedIn(t, "bob", bobPassword)

				return h.do("PUT", "/api/me/email", withLang(`"email":"bob@example.com","current":`+quote(bobPassword), lang), ip, bob)
			},
			english: mail.Message{
				To:      "bob@example.com",
				Subject: "Confirm your email address for Filebox",
				Text: "Hello,\n\nYou asked to use this address for password resets of the Filebox account \"bob\". Open this link within one hour to confirm it:\n\n" +
					testPortalURL + "/confirm-email#TOKEN\n\nThe link works once. If you did not ask for this, ignore this email and nothing changes.\n",
			},
			dutch: mail.Message{
				To:      "bob@example.com",
				Subject: "Bevestig je e-mailadres voor Filebox",
				Text: "Hallo,\n\nJe wilt dit adres gebruiken om het wachtwoord van het account \"bob\" op Filebox opnieuw in te stellen. Open binnen een uur deze link om het te bevestigen:\n\n" +
					testPortalURL + "/confirm-email#TOKEN\n\nDe link werkt maar een keer. Heb je dit niet aangevraagd, negeer deze e-mail dan. Er verandert niets.\n",
			},
		},
	}
}

func (k mailKind) send(t *testing.T, lang string) (mail.Message, string) {
	t.Helper()
	h := newEmailHarness(t)
	if !k.fresh {
		h.confirmedAddress(t, "bob", "bob@example.com")
	}
	rec := k.trigger(h, t, lang, "192.0.2.40")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("%s %s: got %d %s", k.name, lang, rec.Code, rec.Body)
	}
	sent := h.mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("%s %s: expected one email, got %d", k.name, lang, len(sent))
	}
	token := h.lastToken(t)

	return sent[0], token
}

func withToken(want mail.Message, token string) mail.Message {
	want.Text = strings.ReplaceAll(want.Text, "TOKEN", token)

	return want
}

func TestEmailsAreSentInTheChosenLanguage(t *testing.T) {
	for _, kind := range mailKinds() {
		for lang, want := range map[string]mail.Message{`"en"`: kind.english, `"nl"`: kind.dutch, "": kind.english} {
			got, token := kind.send(t, lang)
			if got != withToken(want, token) {
				t.Errorf("%s lang=%s:\n got %+v\nwant %+v", kind.name, lang, got, withToken(want, token))
			}
		}
	}
}

func TestBothLanguagesCarryTheSameLinkAndExpiry(t *testing.T) {
	expiry := map[string][2]string{
		"reset":        {"30 minutes", "30 minuten"},
		"sign-in":      {"15 minutes", "15 minuten"},
		"confirmation": {"one hour", "een uur"},
	}
	for _, kind := range mailKinds() {
		english, _ := kind.send(t, `"en"`)
		dutch, _ := kind.send(t, `"nl"`)
		for i, got := range []mail.Message{english, dutch} {
			if !strings.Contains(got.Text, expiry[kind.name][i]) {
				t.Errorf("%s: mail %d lacks the expiry %q", kind.name, i, expiry[kind.name][i])
			}
			if !strings.Contains(got.Text, testPortalURL+kind.path) {
				t.Errorf("%s: mail %d lacks the link", kind.name, i)
			}
			if got.To != "bob@example.com" {
				t.Errorf("%s: mail %d went to %q", kind.name, i, got.To)
			}
		}
	}
}

func TestMailTextIsPlainAndFreeOfDashesAndAccents(t *testing.T) {
	for _, kind := range mailKinds() {
		for _, lang := range []string{`"en"`, `"nl"`} {
			got, _ := kind.send(t, lang)
			for _, text := range []string{got.Subject, got.Text} {
				for _, r := range text {
					if r > 126 || (r < 32 && r != '\n') {
						t.Errorf("%s %s: odd character %q", kind.name, lang, r)
					}
				}
				if strings.Contains(text, "--") || strings.Contains(text, "<") {
					t.Errorf("%s %s: dash or markup in %q", kind.name, lang, text)
				}
			}
			if strings.Contains(got.Subject, "bob") {
				t.Errorf("%s %s: the subject names the account: %q", kind.name, lang, got.Subject)
			}
		}
	}
}

func TestOnlyTheTwoExactLanguageCodesChangeTheLanguage(t *testing.T) {
	hostile := []string{
		`"de"`, `"NL"`, `"Nl"`, `" nl"`, `"nl "`, `"nl\n"`, `"nl\r\nBcc: attacker@example.com"`, `"nl,en"`, `"nld"`, `"nl-NL"`,
		`""`, `"\u006e\u006c "`, `"ｎｌ"`, `"nl\u0000"`, `"` + strings.Repeat("nl", 1000) + `"`, `"` + strings.Repeat(`é`, 300) + `"`,
		`["nl"]`, `[]`, `{"nl":true}`, `{}`, `1`, `0`, `true`, `false`, `null`, `1.5`, `"Subject: x"`,
	}
	for _, kind := range mailKinds() {
		for _, lang := range hostile {
			got, token := kind.send(t, lang)
			if want := withToken(kind.english, token); got != want {
				t.Errorf("%s lang=%.40s:\n got %+v\nwant %+v", kind.name, lang, got, want)
			}
			if strings.Contains(got.Subject, "\r") || strings.Contains(got.Subject, "\n") {
				t.Errorf("%s lang=%.40s: control characters in the subject", kind.name, lang)
			}
		}
	}
}

func TestTheAnswerIsTheSameForEveryLanguageAndEveryAccount(t *testing.T) {
	attempts := []struct{ name, path, fields string }{
		{"forgot", "/api/forgot", `"username":"bob"`},
		{"forgot unknown", "/api/forgot", `"username":"nobody"`},
		{"forgot unconfirmed", "/api/forgot", `"username":"cleo"`},
		{"link", "/api/sign-in-link", `"email":"bob@example.com"`},
		{"link unknown", "/api/sign-in-link", `"email":"nobody@example.com"`},
	}
	for _, attempt := range attempts {
		var first string
		for i, lang := range []string{"", `"en"`, `"nl"`, `"de"`, `["nl"]`, `null`} {
			h := newEmailHarness(t)
			h.confirmedAddress(t, "bob", "bob@example.com")
			rec := h.do("POST", attempt.path, withLang(attempt.fields, lang), "192.0.2.41", nil)
			answer := rec.Result().Status + rec.Body.String()
			if i == 0 {
				first = answer
			}
			if answer != first {
				t.Errorf("%s lang=%s answered %q, not %q", attempt.name, lang, answer, first)
			}
		}
	}

	unknown := newEmailHarness(t)
	known := newEmailHarness(t)
	known.confirmedAddress(t, "bob", "bob@example.com")
	for _, lang := range []string{`"nl"`, `"en"`} {
		a := unknown.do("POST", "/api/forgot", withLang(`"username":"nobody"`, lang), "192.0.2.42", nil)
		b := known.do("POST", "/api/forgot", withLang(`"username":"bob"`, lang), "192.0.2.42", nil)
		if a.Code != b.Code || a.Body.String() != b.Body.String() {
			t.Errorf("lang=%s: the answer tells accounts apart: %q vs %q", lang, a.Body, b.Body)
		}
	}
	if len(unknown.mail.Sent()) != 0 {
		t.Error("mail went out for an account that does not exist")
	}
}

func TestAnUnknownLanguageStillRejectsMalformedBodiesAsBefore(t *testing.T) {
	h := newEmailHarness(t)
	for _, body := range []string{`{"username":"bob","lang":}`, `{"username":"bob","lang":"nl"`, `[`} {
		if rec := h.do("POST", "/api/forgot", body, "192.0.2.43", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d", body, rec.Code)
		}
	}
}

func TestAnOversizedLanguageValueIsRefusedLikeAnyOversizedBody(t *testing.T) {
	h := newEmailHarness(t)
	h.confirmedAddress(t, "bob", "bob@example.com")
	body := withLang(`"username":"bob"`, `"`+strings.Repeat("nl", maxBodyBytes)+`"`)
	if rec := h.do("POST", "/api/forgot", body, "192.0.2.44", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("got %d", rec.Code)
	}
	if len(h.mail.Sent()) != 0 {
		t.Error("mail went out for a refused request")
	}
}
