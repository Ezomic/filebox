package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzPortalEmailRequests(f *testing.F) {
	for _, seed := range []string{
		`{"username":"bob"}`, `{"username":"nobody"}`, `{"username":1}`, `{}`, ``, `[`, `{"username":"bob\nBcc: x@example.com"}`,
		`{"token":"` + strings.Repeat("A", 43) + `","password":"` + resetToPassword + `"}`,
		`{"token":"","password":""}`, `{"token":1}`, `{"email":"a@b.co","current":"` + bobPassword + `"}`,
		`{"email":"a@b.co\r\nBcc: x@example.com","current":"` + bobPassword + `"}`, `{"email":"a@b@c.co","current":"x"}`,
		`{"email":"bob@example.com"}`, `{"email":"BOB@Example.COM "}`, `{"email":"amy@example.com"}`, `{"email":"bob＠example.com"}`,
		`{"email":"bob@example.com
Bcc: x@example.com"}`, `{"email":1}`,
		`{"token":"` + strings.Repeat("A", 43) + `"}`, `{"token":""}`, `{"token":"` + strings.Repeat("A", 44) + `"}`,
		`{"username":"bob@example.com","password":"` + bobPassword + `"}`, `{"username":" BOB@example.com ","password":"` + bobPassword + `"}`,
		`{"username":"bob@example.com","password":"nope"}`, `{"username":"bob＠example.com","password":"x"}`,
	} {
		f.Add(seed, uint8(0))
		f.Add(seed, uint8(1))
		f.Add(seed, uint8(2))
		f.Add(seed, uint8(3))
		f.Add(seed, uint8(4))
		f.Add(seed, uint8(5))
		f.Add(seed, uint8(6))
	}
	f.Fuzz(func(t *testing.T, body string, op uint8) {
		h := newEmailHarness(t)
		h.confirmedAddress(t, "bob", "bob@example.com")
		bob := h.signedIn(t, "bob", bobPassword)

		var status int
		var answer string
		switch op % 7 {
		case 0:
			rec := h.do("POST", "/api/forgot", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
		case 1:
			rec := h.do("POST", "/api/reset", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
		case 2:
			rec := h.do("POST", "/api/email/confirm", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
		case 3:
			rec := h.do("PUT", "/api/me/email", body, "192.0.2.10", bob)
			status, answer = rec.Code, rec.Body.String()
		case 4:
			rec := h.do("POST", "/api/sign-in-link", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
		case 5:
			rec := h.do("POST", "/api/sign-in", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
			if portalSession(rec) != nil {
				t.Fatalf("op 5 %q signed in without a link", body)
			}
		case 6:
			rec := h.do("POST", "/api/login", body, "192.0.2.10", nil)
			status, answer = rec.Code, rec.Body.String()
			var creds portalCredentials
			if portalSession(rec) != nil && (json.Unmarshal([]byte(body), &creds) != nil || creds.Password != bobPassword) {
				t.Fatalf("op 6 %q signed in with the wrong password", body)
			}
		}

		if status >= 500 {
			t.Fatalf("op %d %q gave a server error %d", op%7, body, status)
		}
		for name, password := range h.spy.Passwords {
			if password != "" {
				t.Fatalf("op %d %q changed the password of %s without a link", op%7, body, name)
			}
		}
		for _, message := range h.mail.Sent() {
			if strings.ContainsAny(message.To, "\r\n,; <>") || strings.Count(message.To, "@") != 1 {
				t.Fatalf("op %d %q mailed the odd address %q", op%7, body, message.To)
			}
			if token := tokenInMail.FindStringSubmatch(message.Text); token != nil && strings.Contains(answer, token[1]) {
				t.Fatalf("op %d %q returned a link token to the requester", op%7, body)
			}
		}
		if got, _ := h.contacts.Get("amy"); got.Email != "" {
			t.Fatalf("op %d %q stored an address for someone else", op%7, body)
		}
	})
}
