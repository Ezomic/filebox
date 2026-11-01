package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	signInLinkTTL = 15 * time.Minute

	signInAnswer = "If that address belongs to an account, we sent a link."

	// signInDecoy is checked in place of an address that belongs to no account, so a wrong address
	// costs the same password check as a wrong password. Its result is never used.
	signInDecoy = "filebox-no-such-account"
)

func (p *Portal) signInRoutes() {
	p.mux.HandleFunc("GET /sign-in", p.staticPage("sign-in.html"))
	p.mux.HandleFunc("GET /sign-in-link", p.staticPage("sign-in-link.html"))
	p.mux.HandleFunc("POST /api/sign-in-link", p.requestSignInLink)
	p.mux.HandleFunc("POST /api/sign-in", p.signInWithLink)
}

// signInIdentity is who a sign-in form field points at. limitKey is what the per-account limit counts
// against: the account for a username or a known address, so both spellings share one budget.
type signInIdentity struct {
	account  string
	limitKey string
	real     bool
}

// signInTarget accepts a username as typed, or a confirmed email address (trimmed, case-insensitive).
// Anything else is refused. An address nobody has confirmed gets the decoy account.
func (p *Portal) signInTarget(identifier string) (signInIdentity, bool) {
	if system.ValidUsername(identifier) {
		return signInIdentity{account: identifier, limitKey: identifier, real: true}, true
	}
	address := strings.TrimSpace(identifier)
	if !validEmail(address) {
		return signInIdentity{}, false
	}
	if contacts := p.email.contacts; contacts != nil {
		if user, found := contacts.Lookup(address); found {
			return signInIdentity{account: user, limitKey: user, real: true}, true
		}
	}

	return signInIdentity{account: signInDecoy, limitKey: "email:" + strings.ToLower(address)}, true
}

type signInLinkBody struct {
	Email string `json:"email"`
}

// The answer is the same for every address and is written before any lookup or mail, like the reset
// request, so it reveals nothing about which addresses are confirmed.
func (p *Portal) requestSignInLink(w http.ResponseWriter, r *http.Request) {
	var body signInLinkBody
	if !decodeJSON(w, r, &body) {
		return
	}
	e := p.email
	ip := ipKey(r)
	if !e.ipLimit.reserve("link:" + ip) {
		log.Printf("portal: sign-in link blocked ip=%s", ip)
		writeError(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}

	address := strings.TrimSpace(body.Email)
	if e.enabled() && validEmail(address) {
		e.background(func() { p.sendSignInLink(address, ip) })
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": signInAnswer})
}

func (p *Portal) sendSignInLink(address, ip string) {
	e := p.email
	user, found := e.contacts.Lookup(address)
	if !found || p.roles.IsAdmin(user) || !p.userExists(user) {
		return
	}
	contact, found := e.contacts.Get(user)
	if !found {
		return
	}
	if !e.userLimit.reserve("signin:" + user) {
		return
	}
	if !e.totalLimit.reserve("all") {
		log.Print("portal: sign-in mail skipped, hourly cap reached")
		return
	}

	token, err := e.tokens.issue(kindSignIn, user, contact.Email, p.sessions.generation(user), signInLinkTTL)
	if err != nil {
		log.Printf("portal: sign-in link for %s: %v", logName(user), err)
		return
	}
	text := "Hello,\n\nSomeone asked for a link to sign in to the Filebox account \"" + user +
		"\". Open this link within 15 minutes and press the button to sign in:\n\n" + e.portalURL + "/sign-in#" + token +
		"\n\nThe link works once and does not change your password. If you did not ask for this, ignore this email.\n"
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := e.mail.Send(ctx, contact.Email, "Your Filebox sign-in link", text); err != nil {
		log.Printf("portal: sign-in email for %s not sent: %v", logName(user), err)
		return
	}
	log.Printf("portal: sign-in email sent user=%s ip=%s", logName(user), ip)
}

// The token is bound to the account that asked and to the address it was mailed to. It is refused if the
// password changed, the address moved or stopped being the account's only confirmed one, the account is gone,
// or it became an admin.
func (p *Portal) signInWithLink(w http.ResponseWriter, r *http.Request) {
	var body linkTokenBody
	if !decodeJSON(w, r, &body) || !p.allowTry(w, r) {
		return
	}

	e := p.email
	link, ok := e.tokens.take(kindSignIn, body.Token)
	if !ok || !e.enabled() || !p.signInLinkUsable(link) {
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	}
	if !p.startSession(w, link.user, link.generation) {
		return
	}
	e.tokens.purgeKinds(link.user, kindSignIn, kindReset)
	log.Printf("portal: login by email link user=%s ip=%s", logName(link.user), ipKey(r))
}

func (p *Portal) signInLinkUsable(link linkToken) bool {
	if link.generation != p.sessions.generation(link.user) || p.roles.IsAdmin(link.user) || !p.userExists(link.user) {
		return false
	}
	owner, found := p.email.contacts.Lookup(link.email)

	return found && owner == link.user
}
