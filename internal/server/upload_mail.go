package server

import (
	"context"
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

type uploadNotices struct {
	mu    sync.Mutex
	links map[string]*uploadNotice
	after func(wait time.Duration, run func())
}

type uploadNotice struct {
	owner     string
	folder    string
	lang      string
	arrived   int
	sent      time.Time
	scheduled bool
}

// noteArrival counts a file that came in through an upload link and makes sure its owner hears about it: at most one
// summary per link per hour, and only when the portal can send mail.
func (p *Portal) noteArrival(link auth.Share) {
	if !p.email.enabled() {
		return
	}
	n := &p.receive.notices
	n.mu.Lock()
	defer n.mu.Unlock()

	now := p.now()
	for name, old := range n.links {
		if !old.scheduled && now.Sub(old.sent) >= uploadMailGap {
			delete(n.links, name)
		}
	}
	notice := n.links[link.Name]
	if notice == nil {
		notice = &uploadNotice{}
		n.links[link.Name] = notice
	}
	notice.owner, notice.folder, notice.lang = link.Owner, link.Path, link.Lang
	notice.arrived++
	if notice.scheduled {
		return
	}
	notice.scheduled = true
	n.after(max(uploadMailSettle, notice.sent.Add(uploadMailGap).Sub(now)), func() { p.sendUploadSummary(link.Name) })
}

func (p *Portal) sendUploadSummary(name string) {
	n := &p.receive.notices
	n.mu.Lock()
	notice, found := n.links[name]
	if !found {
		n.mu.Unlock()
		return
	}
	summary := *notice
	notice.arrived, notice.scheduled, notice.sent = 0, false, p.now()
	n.mu.Unlock()

	contact, found := p.email.contacts.Get(summary.owner)
	if summary.arrived == 0 || !found || !contact.Confirmed || !p.userExists(summary.owner) {
		return
	}
	message := chosenMailLang(summary.lang).uploadSummary(summary.arrived, path.Base(summary.folder), p.email.portalURL+"/#"+url.PathEscape(summary.folder))
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := p.email.mail.Send(ctx, contact.Email, message.subject, message.body); err != nil {
		log.Printf("portal: upload link summary for %s not sent: %v", logName(summary.owner), err)
		return
	}
	log.Printf("portal: upload link summary sent user=%s files=%d", logName(summary.owner), summary.arrived)
}
