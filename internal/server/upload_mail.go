package server

import (
	"context"
	"errors"
	"log"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
)

const (
	// The first file of a quiet link waits this long, so files sent together end up in one summary.
	uploadMailSettle = 10 * time.Minute
	uploadMailGap    = time.Hour
)

var errNoConfirmedAddress = errors.New("the owner has no confirmed address")

// uploadNotices only knows which links have a summary planned. What a summary says, and when the last one went out,
// is kept with each link, so it survives a restart.
type uploadNotices struct {
	mu        sync.Mutex
	scheduled map[string]bool
	after     func(wait time.Duration, run func())
}

// summaryDue is when the next summary of a link may go out: a little after its first unreported file, and never
// within the hour after the one before.
func summaryDue(link auth.Share) time.Time {
	due := link.UnreportedSince.Add(uploadMailSettle)
	if next := link.Reported.Add(uploadMailGap); next.After(due) {
		due = next
	}

	return due
}

// ResumeUploadSummaries plans the summaries a restart interrupted.
func (p *Portal) ResumeUploadSummaries() {
	if p.shares == nil {
		return
	}
	for _, link := range p.shares.All() {
		if link.IsUploadLink() {
			p.scheduleSummary(link, time.Time{})
		}
	}
}

// scheduleSummary plans one summary of the files that arrived through a link, never before notBefore. It does
// nothing when the portal cannot send mail or a summary is already planned.
func (p *Portal) scheduleSummary(link auth.Share, notBefore time.Time) {
	if !p.email.enabled() || link.Unreported == 0 {
		return
	}
	n := &p.receive.notices
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.scheduled[link.Name] {
		return
	}
	n.scheduled[link.Name] = true
	due := summaryDue(link)
	if notBefore.After(due) {
		due = notBefore
	}
	n.after(max(0, due.Sub(p.now())), func() { p.sendUploadSummary(link.Name) })
}

// sendUploadSummary takes the count off the link only once the email went out. A failed send, or an owner without
// a confirmed address, keeps the count for the next try an hour later.
func (p *Portal) sendUploadSummary(name string) {
	n := &p.receive.notices
	n.mu.Lock()
	delete(n.scheduled, name)
	n.mu.Unlock()

	link, found := p.shares.Get(name)
	if !found || !link.IsUploadLink() || link.Unreported == 0 {
		return
	}
	now := p.now()
	if now.Before(summaryDue(link)) {
		p.scheduleSummary(link, time.Time{})
		return
	}
	if err := p.mailSummary(link); err != nil {
		if !errors.Is(err, errNoConfirmedAddress) {
			log.Printf("portal: upload link summary for %s not sent: %v", logName(link.Owner), err)
		}
		p.scheduleSummary(link, now.Add(uploadMailGap))
		return
	}
	reported, err := p.shares.UpdateUploadLink(name, func(saved *auth.Share) error {
		saved.Unreported = max(0, saved.Unreported-link.Unreported)
		saved.Reported, saved.UnreportedSince = now, time.Time{}
		if saved.Unreported > 0 {
			saved.UnreportedSince = now
		}
		return nil
	})
	if err != nil {
		log.Printf("portal: save upload link summary: %v", err)
		return
	}
	log.Printf("portal: upload link summary sent user=%s files=%d", logName(link.Owner), link.Unreported)
	p.scheduleSummary(reported, time.Time{})
}

// The link points at the folder in the file browser, which reads its #path with decodeURIComponent, so a nested
// folder's slashes may arrive as %2F.
func (p *Portal) mailSummary(link auth.Share) error {
	contact, found := p.email.contacts.Get(link.Owner)
	if !found || !contact.Confirmed || !p.userExists(link.Owner) {
		return errNoConfirmedAddress
	}
	message := chosenMailLang(link.Lang).uploadSummary(link.Unreported, path.Base(link.Path), p.email.portalURL+"/#"+url.PathEscape(link.Path))
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	return p.email.mail.Send(ctx, contact.Email, message.subject, message.body)
}
