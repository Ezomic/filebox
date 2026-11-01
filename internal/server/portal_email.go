package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/mail"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	resetLinkTTL   = 30 * time.Minute
	confirmLinkTTL = time.Hour

	emailIPRequests  = 5
	emailUserSends   = 3
	emailTotalSends  = 15
	emailTryRequests = 20
	emailMailWindow  = time.Hour
	maxEmailLength   = 254
	maxSendsInFlight = 4
	sendTimeout      = 15 * time.Second

	forgotAnswer = "If that account exists and has a confirmed address, we sent an email."
	linkInvalid  = "this link is not valid or has expired"
)

// Contacts holds each user's own email address and whether they confirmed it.
type Contacts interface {
	Get(user string) (auth.Contact, bool)
	Set(user, email string) error
	Confirm(user, email string) (bool, error)
	Lookup(email string) (string, bool)
	Forget(user string) error
}

// emailFlow is everything the email address and password reset pages need. It stays off unless mail
// can actually be sent and the links have a configured host to point at.
type emailFlow struct {
	contacts  Contacts
	mail      mail.Sender
	portalURL string
	tokens    *linkTokens

	ipLimit    *loginLimiter
	userLimit  *loginLimiter
	totalLimit *loginLimiter
	tryLimit   *loginLimiter
	// addressLimit counts failed sign-ins by an address that is no account's, apart from userLimit so
	// throwaway addresses cannot push real accounts out of that map.
	addressLimit *loginLimiter

	background func(work func())
}

func newEmailFlow(deps PortalDeps) *emailFlow {
	slots := make(chan struct{}, maxSendsInFlight)

	return &emailFlow{
		contacts:     deps.Contacts,
		mail:         deps.Mail,
		portalURL:    strings.TrimRight(deps.PortalURL, "/"),
		tokens:       newLinkTokens(),
		ipLimit:      newLoginLimiter(emailIPRequests, failWindow),
		userLimit:    newLoginLimiter(emailUserSends, emailMailWindow),
		totalLimit:   newLoginLimiter(emailTotalSends, emailMailWindow),
		tryLimit:     newLoginLimiter(emailTryRequests, failWindow),
		addressLimit: newLoginLimiter(portalUserFails, failWindow),
		background: func(work func()) {
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					work()
				}()
			default:
				log.Print("portal: reset mail dropped, too many sends in flight")
			}
		},
	}
}

func (e *emailFlow) enabled() bool {
	return e.contacts != nil && e.mail != nil && e.portalURL != ""
}

func (p *Portal) emailRoutes() {
	p.mux.HandleFunc("GET /forgot", p.staticPage("forgot.html"))
	p.mux.HandleFunc("GET /reset", p.staticPage("reset.html"))
	p.mux.HandleFunc("GET /confirm-email", p.staticPage("confirm-email.html"))
	p.mux.HandleFunc("GET /api/forgot", p.forgotStatus)
	p.mux.HandleFunc("POST /api/forgot", p.forgot)
	p.mux.HandleFunc("POST /api/reset", p.resetPassword)
	p.mux.HandleFunc("POST /api/email/confirm", p.confirmEmail)
	p.mux.HandleFunc("GET /api/me/email", p.requireSession(p.myEmail))
	p.mux.HandleFunc("PUT /api/me/email", p.requireSession(p.setMyEmail))
	p.signInRoutes()
}

func (p *Portal) staticPage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, p.pages, name)
	}
}

func (p *Portal) forgotStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": p.email.enabled()})
}

type emailState struct {
	Enabled   bool   `json:"enabled"`
	Email     string `json:"email"`
	Confirmed bool   `json:"confirmed"`
}

func (p *Portal) myEmail(w http.ResponseWriter, _ *http.Request, user string) {
	if !p.email.enabled() {
		writeJSON(w, http.StatusOK, emailState{})
		return
	}
	contact, _ := p.email.contacts.Get(user)
	writeJSON(w, http.StatusOK, emailState{Enabled: true, Email: contact.Email, Confirmed: contact.Confirmed})
}

type emailChange struct {
	Email   string `json:"email"`
	Current string `json:"current"`
}

func (p *Portal) setMyEmail(w http.ResponseWriter, r *http.Request, user string) {
	var body emailChange
	if !decodeJSON(w, r, &body) {
		return
	}
	e := p.email
	if !e.enabled() {
		writeError(w, http.StatusServiceUnavailable, "email is not set up on this server, ask the owner")
		return
	}
	address := strings.TrimSpace(body.Email)
	if !validEmail(address) {
		writeError(w, http.StatusUnprocessableEntity, "enter one plain email address such as name@example.com")
		return
	}
	if !p.confirmPassword(w, r, user, body.Current, "email change", "your current password is wrong") {
		return
	}

	existing, found := e.contacts.Get(user)
	if found && existing.Confirmed && strings.EqualFold(existing.Email, address) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !e.userLimit.reserve("confirm:"+user) || !e.userLimit.reserve("to:"+strings.ToLower(address)) || !e.totalLimit.reserve("all") {
		writeError(w, http.StatusTooManyRequests, "too many emails sent, try again later")
		return
	}
	if !found || existing.Email != address {
		if err := e.contacts.Set(user, address); err != nil {
			log.Printf("portal: save email for %s: %v", logName(user), err)
			writeError(w, http.StatusInternalServerError, "could not save the address")
			return
		}
	}
	e.tokens.purgeUser(user)

	token, err := e.tokens.issue(kindConfirm, user, address, 0, confirmLinkTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the confirmation")
		return
	}
	text := "Hello,\n\nYou asked to use this address for password resets of the Filebox account \"" + user +
		"\". Open this link within one hour to confirm it:\n\n" + e.portalURL + "/confirm-email#" + token +
		"\n\nThe link works once. If you did not ask for this, ignore this email and nothing changes.\n"
	if err := p.sendMail(r.Context(), address, "Confirm your email address for Filebox", text); err != nil {
		log.Printf("portal: confirmation email for %s not sent: %v", logName(user), err)
		writeError(w, http.StatusServiceUnavailable, "could not send the email right now, try again later")
		return
	}
	log.Printf("portal: confirmation email sent user=%s ip=%s", logName(user), ipKey(r))
	w.WriteHeader(http.StatusNoContent)
}

type linkTokenBody struct {
	Token string `json:"token"`
}

func (p *Portal) confirmEmail(w http.ResponseWriter, r *http.Request) {
	var body linkTokenBody
	if !decodeJSON(w, r, &body) || !p.allowTry(w, r) {
		return
	}

	link, ok := p.email.tokens.take(kindConfirm, body.Token)
	if !ok || !p.email.enabled() {
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	}
	confirmed, err := p.email.contacts.Confirm(link.user, link.email)
	switch {
	case err != nil:
		log.Printf("portal: confirm email for %s: %v", logName(link.user), err)
		writeError(w, http.StatusInternalServerError, "could not confirm the address")
		return
	case !confirmed:
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	}
	log.Printf("portal: email confirmed user=%s ip=%s", logName(link.user), ipKey(r))
	w.WriteHeader(http.StatusNoContent)
}

type forgotBody struct {
	Username string `json:"username"`
}

// The answer is the same for every username and is written before any lookup or mail, so it reveals
// nothing about which accounts exist or have an address. Only the caller's own IP can change it.
func (p *Portal) forgot(w http.ResponseWriter, r *http.Request) {
	var body forgotBody
	if !decodeJSON(w, r, &body) {
		return
	}
	e := p.email
	ip := ipKey(r)
	if !e.ipLimit.reserve(ip) {
		log.Printf("portal: forgot password blocked ip=%s", ip)
		writeError(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}

	name := body.Username
	if e.enabled() && system.ValidUsername(name) {
		e.background(func() { p.sendResetLink(name, ip) })
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": forgotAnswer})
}

func (p *Portal) sendResetLink(name, ip string) {
	e := p.email
	contact, found := e.contacts.Get(name)
	if !found || !contact.Confirmed || !p.userExists(name) {
		return
	}
	if !e.userLimit.reserve(name) {
		return
	}
	if !e.totalLimit.reserve("all") {
		log.Print("portal: reset mail skipped, hourly cap reached")
		return
	}

	token, err := e.tokens.issue(kindReset, name, contact.Email, p.sessions.generation(name), resetLinkTTL)
	if err != nil {
		log.Printf("portal: reset link for %s: %v", logName(name), err)
		return
	}
	text := "Hello,\n\nSomeone asked to reset the password of the Filebox account \"" + name +
		"\". Open this link within 30 minutes to choose a new password:\n\n" + e.portalURL + "/reset#" + token +
		"\n\nThe link works once. If you did not ask for this, ignore this email and your password stays as it is.\n"
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := e.mail.Send(ctx, contact.Email, "Reset your Filebox password", text); err != nil {
		log.Printf("portal: reset email for %s not sent: %v", logName(name), err)
		return
	}
	log.Printf("portal: reset email sent user=%s ip=%s", logName(name), ip)
}

func (p *Portal) sendMail(ctx context.Context, to, subject, text string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	return p.email.mail.Send(ctx, to, subject, text)
}

func (p *Portal) userExists(name string) bool {
	names, err := p.users.List()

	return err == nil && slices.Contains(names, name)
}

type resetBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

func (p *Portal) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body resetBody
	if !decodeJSON(w, r, &body) || !validPassword(w, body.Password) || !p.allowTry(w, r) {
		return
	}

	link, ok := p.email.tokens.take(kindReset, body.Token)
	if !ok || !p.email.enabled() || link.generation != p.sessions.generation(link.user) {
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	}
	contact, found := p.email.contacts.Get(link.user)
	if !found || !contact.Confirmed || !strings.EqualFold(contact.Email, link.email) {
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	}

	switch err := p.users.SetPassword(link.user, body.Password); {
	case errors.Is(err, system.ErrUserNotFound):
		writeError(w, http.StatusBadRequest, linkInvalid)
		return
	case err != nil:
		log.Printf("portal: reset password for %s: %v", logName(link.user), err)
		writeError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	p.sessions.destroyUser(link.user)
	p.email.tokens.purgeUser(link.user)
	log.Printf("portal: password reset by email user=%s ip=%s", logName(link.user), ipKey(r))
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) allowTry(w http.ResponseWriter, r *http.Request) bool {
	if p.email.tryLimit.reserve(ipKey(r)) {
		return true
	}
	writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")

	return false
}

// dropIfGone forgets the address of an account that no longer exists, so a later account with the same
// name cannot inherit it and be reset through someone else's mailbox.
func (e *emailFlow) dropIfGone(user string, users system.Users) {
	if e.contacts == nil {
		return
	}
	if _, found := e.contacts.Get(user); !found {
		return
	}
	names, err := users.List()
	if err != nil || slices.Contains(names, user) {
		return
	}
	e.tokens.purgeUser(user)
	if err := e.contacts.Forget(user); err != nil {
		log.Printf("portal: forget email of %s: %v", logName(user), err)
	}
}

const localSpecials = ".!#$%&'*+/=?^_`{|}~-"

// An address is one plain ASCII mailbox, which is also what the mail service accepts.
// It is stored as typed (after trimming) and compared case-insensitively.
func validEmail(address string) bool {
	if len(address) > maxEmailLength {
		return false
	}
	local, domain, ok := strings.Cut(address, "@")
	if !ok || local == "" || len(local) > 64 || strings.Contains(domain, "@") {
		return false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for i := range len(local) {
		c := local[i]
		if !isAlnum(c) && !strings.ContainsRune(localSpecials, rune(c)) {
			return false
		}
	}

	return validDomain(domain)
}

func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			if !isAlnum(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]

	return strings.TrimLeft(last, "0123456789") != ""
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
