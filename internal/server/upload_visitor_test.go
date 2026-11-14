package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

var visitorEpoch = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func linkWithPending(name string, ids ...string) auth.Share {
	pending := map[string]auth.PendingUpload{}
	for _, id := range ids {
		pending[id] = auth.PendingUpload{}
	}

	return auth.Share{Name: name, Kind: auth.UploadLink, Pending: pending}
}

func TestClosedUploadsAreRememberedPerLinkForTheirMaxAge(t *testing.T) {
	rc := newReceiving()
	rc.rememberClosed(linkWithPending("drop-box", "up-1", "up-2"), visitorEpoch)

	if !rc.wasClosed("up-1", "drop-box", visitorEpoch) || !rc.wasClosed("up-2", "drop-box", visitorEpoch.Add(system.VisitorUploadMaxAge)) {
		t.Fatal("a closed upload must be remembered for its whole max age")
	}
	if rc.wasClosed("up-1", "other-link", visitorEpoch) {
		t.Fatal("a closed upload belongs to the link it was sent through")
	}
	if rc.wasClosed("up-1", "drop-box", visitorEpoch.Add(system.VisitorUploadMaxAge+time.Second)) {
		t.Fatal("a closed upload must be forgotten after its max age")
	}
	if rc.wasClosed("up-3", "drop-box", visitorEpoch) {
		t.Fatal("an upload that was never closed must not count as closed")
	}
}

func TestClosedUploadsOverTheCapDropExpiredOnesFirst(t *testing.T) {
	rc := newReceiving()
	old := visitorEpoch.Add(-2 * system.VisitorUploadMaxAge)
	for i := range maxClosedUploads {
		rc.closed[fmt.Sprintf("old-%d", i)] = closedUpload{link: "drop-box", at: old}
	}

	rc.rememberClosed(linkWithPending("drop-box", "new-1"), visitorEpoch)

	if len(rc.closed) != 1 || !rc.wasClosed("new-1", "drop-box", visitorEpoch) {
		t.Fatalf("%d remembered, want only the new upload", len(rc.closed))
	}
}

func TestClosedUploadsStayBoundedWhenNoneHaveExpired(t *testing.T) {
	rc := newReceiving()
	for i := range maxClosedUploads {
		rc.closed[fmt.Sprintf("recent-%d", i)] = closedUpload{link: "drop-box", at: visitorEpoch}
	}

	rc.rememberClosed(linkWithPending("drop-box", "new-1", "new-2", "new-3"), visitorEpoch)

	if len(rc.closed) != maxClosedUploads {
		t.Fatalf("%d remembered, want the cap of %d", len(rc.closed), maxClosedUploads)
	}
}
