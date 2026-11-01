package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

type adminQuotaView struct {
	Name       string  `json:"name"`
	Bytes      *uint64 `json:"bytes"`
	Quota      *uint64 `json:"quota"`
	QuotaReady bool    `json:"quotaReady"`
}

func quotaBody(t *testing.T, bytes any, confirm string) string {
	t.Helper()

	return jsonBody(t, map[string]any{"bytes": bytes, "confirm": confirm})
}

func (h *portalHarness) adminUsers(t *testing.T, cookie *http.Cookie) (users []adminQuotaView, quotas bool) {
	t.Helper()
	rec := h.call("GET", "/api/admin/users", "", cookie)
	var body struct {
		Users  []adminQuotaView `json:"users"`
		Quotas bool             `json:"quotas"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return body.Users, body.Quotas
}

func findUser(t *testing.T, users []adminQuotaView, name string) adminQuotaView {
	t.Helper()
	for _, user := range users {
		if user.Name == name {
			return user
		}
	}
	t.Fatalf("no %s in %+v", name, users)

	return adminQuotaView{}
}

func quotaRoutes() []adminRoute {
	return []adminRoute{
		{"PUT", "/api/admin/users/cleo/quota", `{"bytes":10485760,"confirm":"` + bobPassword + `"}`},
		{"DELETE", "/api/admin/users/cleo/quota", `{"confirm":"` + bobPassword + `"}`},
	}
}

func TestQuotaRoutesRejectSignedOutRequestsAndPlainUsers(t *testing.T) {
	h := newPortalHarness(t)
	_, bob, _ := h.sessions(t)
	calls := h.verifier.Calls

	for _, route := range quotaRoutes() {
		if rec := h.call(route.method, route.path, route.body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("signed out %s: got %d", route.method, rec.Code)
		}
		if rec := h.call(route.method, route.path, route.body, bob); rec.Code != http.StatusForbidden {
			t.Errorf("plain user %s: got %d", route.method, rec.Code)
		}
	}
	if h.spy.mutations != 0 || h.verifier.Calls != calls {
		t.Fatalf("a refused request reached the backend: %d mutations, %d checks", h.spy.mutations, h.verifier.Calls-calls)
	}
}

func TestPlainUserListingIsRefusedSoNobodyReadsAnotherUsersUsage(t *testing.T) {
	h := newPortalHarness(t)
	_, bob, _ := h.sessions(t)
	h.spy.SetUsed("cleo", 7<<20)

	rec := h.call("GET", "/api/admin/users", "", bob)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "cleo") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminSetsShowsAndClearsAQuota(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.spy.SetUsed("cleo", 3<<20)

	users, quotas := h.adminUsers(t, amy)
	cleo := findUser(t, users, "cleo")
	if !quotas || cleo.Quota != nil || !cleo.QuotaReady || cleo.Bytes == nil || *cleo.Bytes != 3<<20 {
		t.Fatalf("a new user has no quota: %+v", cleo)
	}

	rec := h.call("PUT", "/api/admin/users/cleo/quota", quotaBody(t, 10485760, amyPassword), amy)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	users, _ = h.adminUsers(t, amy)
	if cleo = findUser(t, users, "cleo"); cleo.Quota == nil || *cleo.Quota != 10<<20 {
		t.Fatalf("got %+v", cleo)
	}

	rec = h.call("DELETE", "/api/admin/users/cleo/quota", jsonBody(t, map[string]any{"confirm": amyPassword}), amy)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	users, _ = h.adminUsers(t, amy)
	if cleo = findUser(t, users, "cleo"); cleo.Quota != nil {
		t.Fatalf("got %+v", cleo)
	}
}

func TestQuotaChangesNeedTheAdminsOwnPassword(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	cases := map[string]string{
		"missing":          quotaBody(t, 10485760, ""),
		"wrong":            quotaBody(t, 10485760, "not my password"),
		"someone elses":    quotaBody(t, 10485760, bobPassword),
		"wrong type":       `{"bytes":10485760,"confirm":5}`,
		"no confirm field": `{"bytes":10485760}`,
	}
	for name, body := range cases {
		rec := h.call("PUT", "/api/admin/users/cleo/quota", body, amy)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	rec := h.call("DELETE", "/api/admin/users/cleo/quota", jsonBody(t, map[string]any{"confirm": "nope nope nope"}), amy)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("clear with a wrong password: got %d", rec.Code)
	}
	if h.spy.mutations != 0 {
		t.Fatalf("a quota changed without the admin's password: %d mutations", h.spy.mutations)
	}
}

func TestAdminQuotaRefusesBadValuesBeforeAskingForThePassword(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	calls := h.verifier.Calls

	bodies := map[string]int{
		`{"confirm":"` + amyPassword + `"}`:                             http.StatusUnprocessableEntity,
		`{"bytes":null,"confirm":"` + amyPassword + `"}`:                http.StatusUnprocessableEntity,
		`{"bytes":0,"confirm":"` + amyPassword + `"}`:                   http.StatusUnprocessableEntity,
		`{"bytes":-5,"confirm":"` + amyPassword + `"}`:                  http.StatusUnprocessableEntity,
		`{"bytes":1048575,"confirm":"` + amyPassword + `"}`:             http.StatusUnprocessableEntity,
		`{"bytes":1125899906842625,"confirm":"` + amyPassword + `"}`:    http.StatusUnprocessableEntity,
		`{"bytes":9223372036854775808,"confirm":"` + amyPassword + `"}`: http.StatusBadRequest,
		`{"bytes":1e9,"confirm":"` + amyPassword + `"}`:                 http.StatusBadRequest,
		`{"bytes":"10GB","confirm":"` + amyPassword + `"}`:              http.StatusBadRequest,
		`{"bytes":10485760.5,"confirm":"` + amyPassword + `"}`:          http.StatusBadRequest,
		`{"bytes":010485760,"confirm":"` + amyPassword + `"}`:           http.StatusBadRequest,
		`{"bytes":"١٠٤٨٥٧٦٠","confirm":"` + amyPassword + `"}`:          http.StatusBadRequest,
	}
	for body, want := range bodies {
		if rec := h.call("PUT", "/api/admin/users/cleo/quota", body, amy); rec.Code != want {
			t.Errorf("%s: got %d, want %d", body, rec.Code, want)
		}
	}
	if h.spy.mutations != 0 || h.verifier.Calls != calls {
		t.Fatalf("a bad value reached the backend: %d mutations, %d checks", h.spy.mutations, h.verifier.Calls-calls)
	}
}

func TestAdminQuotaOnlyForExistingPlainUsers(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	if err := h.roles.Promote("cleo"); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{
		"ghost":    http.StatusNotFound,
		"cleo":     http.StatusForbidden,
		"amy":      http.StatusForbidden,
		"root":     http.StatusUnprocessableEntity,
		"-rf":      http.StatusUnprocessableEntity,
		"A":        http.StatusUnprocessableEntity,
		"..%2Fbob": http.StatusUnprocessableEntity,
	} {
		rec := h.call("PUT", "/api/admin/users/"+name+"/quota", quotaBody(t, 10485760, amyPassword), amy)
		if rec.Code != want && !(want == http.StatusUnprocessableEntity && rec.Code == http.StatusNotFound) {
			t.Errorf("set %s: got %d, want %d", name, rec.Code, want)
		}
		rec = h.call("DELETE", "/api/admin/users/"+name+"/quota", jsonBody(t, map[string]any{"confirm": amyPassword}), amy)
		if rec.Code != want && !(want == http.StatusUnprocessableEntity && rec.Code == http.StatusNotFound) {
			t.Errorf("clear %s: got %d, want %d", name, rec.Code, want)
		}
	}
	if h.spy.mutations != 0 {
		t.Fatalf("a refused target reached the helper: %d", h.spy.mutations)
	}
}

func TestAdminQuotaWhenTheBoxHasNoQuotas(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.spy.QuotasOff = true

	users, quotas := h.adminUsers(t, amy)
	if quotas || len(users) != 3 || findUser(t, users, "bob").QuotaReady {
		t.Fatalf("the list must still work: %+v quotas=%v", users, quotas)
	}
	rec := h.call("PUT", "/api/admin/users/bob/quota", quotaBody(t, 10485760, amyPassword), amy)
	if rec.Code != http.StatusConflict || !strings.Contains(errorMessage(t, rec.Body.String()), "quotas") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminQuotaHelperFailureHidesDetail(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.spy.failWith = errors.New("sudo: secret detail")

	rec := h.call("PUT", "/api/admin/users/bob/quota", quotaBody(t, 10485760, amyPassword), amy)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminQuotaChangesAreAudited(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	logged := captureLog(t)

	h.call("PUT", "/api/admin/users/bob/quota", quotaBody(t, 10485760, amyPassword), amy)
	h.call("DELETE", "/api/admin/users/bob/quota", jsonBody(t, map[string]any{"confirm": amyPassword}), amy)

	for _, want := range []string{
		"portal: admin set-quota by=\"amy\" target=\"bob\" bytes=10485760 ip=192.0.2.10",
		"portal: admin clear-quota by=\"amy\" target=\"bob\" ip=192.0.2.10",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("missing %q in %q", want, logged)
		}
	}
	if strings.Contains(logged.String(), amyPassword) {
		t.Fatal("the password was logged")
	}
}

// overQuotaHarness serves one signed-in user whose files live in a fake that enforces a byte limit.
type overQuotaHarness struct {
	*portalHarness
	files  *system.FakeFiles
	cookie *http.Cookie
}

func newOverQuotaHarness(t *testing.T, limit int64) *overQuotaHarness {
	t.Helper()
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword}}
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := system.NewFakeFiles()
	files.SetLimit(limit)
	portal := NewPortal(PortalDeps{
		Verifier:  verifier,
		Users:     system.NewFakeUsers("amy"),
		Roles:     roles,
		OpenFiles: func(string) (system.Files, error) { return files, nil },
		MaxUpload: 1 << 20,
	})
	base := &portalHarness{portal: portal, verifier: verifier, roles: roles}

	return &overQuotaHarness{portalHarness: base, files: files, cookie: base.signedIn(t, "amy", amyPassword)}
}

func (h *overQuotaHarness) put(path, route, content string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", route, strings.NewReader(content))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "application/octet-stream")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.AddCookie(h.cookie)
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func (h *overQuotaHarness) simpleUpload(path, content string) *httptest.ResponseRecorder {
	return h.put(path, "/api/files/content?path="+url.QueryEscape(path), content, nil)
}

func TestPortalUploadOverQuotaSaysSo(t *testing.T) {
	h := newOverQuotaHarness(t, 10)

	if rec := h.simpleUpload("ok.txt", "123456"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec := h.simpleUpload("big.txt", "123456")
	if rec.Code != http.StatusInsufficientStorage || !strings.Contains(errorMessage(t, rec.Body.String()), "over your storage quota") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPortalChunkedUploadOverQuotaSaysSo(t *testing.T) {
	h := newOverQuotaHarness(t, 10)

	rec := h.do("POST", "/api/uploads", `{"path":"big.bin","size":12}`, "192.0.2.10", h.cookie)
	var created struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	rec = h.put("big.bin", "/api/uploads/"+created.ID, "123456789012", map[string]string{"Content-Range": "bytes 0-11/12"})
	if rec.Code != http.StatusInsufficientStorage || !strings.Contains(errorMessage(t, rec.Body.String()), "over your storage quota") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestLoweringTheQuotaBlocksNewWritesAndDeletesNothing(t *testing.T) {
	h := newOverQuotaHarness(t, 100)
	if rec := h.simpleUpload("keep.txt", "0123456789"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	h.files.SetLimit(4)

	if rec := h.simpleUpload("new.txt", "x"); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec := h.do("GET", "/api/files/download?path=keep.txt", "", "192.0.2.10", h.cookie)
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("existing data must stay readable: %d %q", rec.Code, rec.Body)
	}
	if rec := h.do("DELETE", "/api/files?path=keep.txt", "", "192.0.2.10", h.cookie); rec.Code != http.StatusOK {
		t.Fatalf("deleting must still work over quota: %d %s", rec.Code, rec.Body)
	}
}

func TestDeletingFreesTheSpaceOnlyOnceTheTrashIsEmptied(t *testing.T) {
	h := newOverQuotaHarness(t, 10)
	if rec := h.simpleUpload("a.txt", "0123456789"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	h.do("DELETE", "/api/files?path=a.txt", "", "192.0.2.10", h.cookie)

	if rec := h.simpleUpload("b.txt", "x"); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("trash counts toward the quota: %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/trash", "", "192.0.2.10", h.cookie); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("empty trash got %d %s", rec.Code, rec.Body)
	}
	if rec := h.simpleUpload("b.txt", "x"); rec.Code != http.StatusNoContent {
		t.Fatalf("emptying the trash frees the space: %d %s", rec.Code, rec.Body)
	}
}
