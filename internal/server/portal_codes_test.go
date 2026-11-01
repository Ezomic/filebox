package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type codedReply struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func decodeCoded(t *testing.T, rec *httptest.ResponseRecorder) codedReply {
	t.Helper()
	var reply codedReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("not a JSON body: %q", rec.Body)
	}

	return reply
}

func wantCoded(t *testing.T, rec *httptest.ResponseRecorder, status int, code, english string) {
	t.Helper()
	reply := decodeCoded(t, rec)
	if rec.Code != status || reply.Code != code || reply.Error != english {
		t.Fatalf("got %d code=%q error=%q, want %d code=%q error=%q", rec.Code, reply.Code, reply.Error, status, code, english)
	}
}

func TestLoginErrorsCarryACodeAndKeepTheEnglishText(t *testing.T) {
	h := newPortalHarness(t)
	wantCoded(t, h.login("amy", "not the password", "192.0.2.10"), http.StatusUnauthorized, "invalid_credentials", "wrong username or password")
	wantCoded(t, h.login("nobody", "whatever password", "192.0.2.11"), http.StatusUnauthorized, "invalid_credentials", "wrong username or password")
}

func TestLoginRateLimitCarriesACode(t *testing.T) {
	h := newPortalHarness(t)
	var last *httptest.ResponseRecorder
	for range portalIPFails + 2 {
		last = h.login("amy", "not the password", "192.0.2.12")
	}
	wantCoded(t, last, http.StatusTooManyRequests, "rate_limited", "too many failed attempts, try again later")
}

func TestSessionAndRoleErrorsCarryACode(t *testing.T) {
	h := newPortalHarness(t)
	wantCoded(t, h.call("GET", "/api/me", "", nil), http.StatusUnauthorized, "not_signed_in", "not signed in")
	_, bob, _ := h.sessions(t)
	wantCoded(t, h.call("GET", "/api/admin/users", "", bob), http.StatusForbidden, "admins_only", "admins only")
}

func TestAdminErrorsCarryACode(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	cases := []struct {
		name, method, path, body string
		status                   int
		code, english            string
	}{
		{"bad name", "POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "A!", "password": newPassword}), 422, "username_invalid", "username must be 3 to 32 characters: lowercase letters, digits, - and _, starting with a letter"},
		{"short password", "POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "dora", "password": "short"}), 422, "password_invalid", "password must be 12 to 128 characters on a single line"},
		{"existing name", "POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "bob", "password": newPassword}), 409, "user_exists", "a user with that name already exists"},
		{"wrong own password", "PUT", "/api/admin/users/bob/password", jsonBody(t, map[string]any{"password": newPassword, "confirm": "not mine"}), 401, "wrong_password", "your password is wrong"},
		{"unknown target", "PUT", "/api/admin/users/ghost/password", jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword}), 404, "user_not_found", "no such user"},
		{"own role", "PUT", "/api/admin/users/amy/role", jsonBody(t, map[string]any{"role": "user", "confirm": amyPassword}), 409, "own_admin_role", "you cannot remove your own admin role"},
		{"own account", "DELETE", "/api/admin/users/amy", jsonBody(t, map[string]any{"confirm": amyPassword}), 409, "own_account", "you cannot delete your own account"},
		{"bad role", "PUT", "/api/admin/users/bob/role", jsonBody(t, map[string]any{"role": "king", "confirm": amyPassword}), 422, "role_invalid", "role must be user or admin"},
		{"bad quota", "PUT", "/api/admin/users/bob/quota", jsonBody(t, map[string]any{"bytes": 5, "confirm": amyPassword}), 422, "quota_invalid", "the quota must be a whole number of bytes between 1 MB and 1 PB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantCoded(t, h.call(c.method, c.path, c.body, amy), c.status, c.code, c.english)
		})
	}
}

func TestChangingAnAdminsPasswordOrQuotaIsRefusedWithACode(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	if err := h.roles.Promote("bob"); err != nil {
		t.Fatal(err)
	}
	wantCoded(t, h.call("PUT", "/api/admin/users/bob/password", jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword}), amy),
		http.StatusForbidden, "admin_protected", "admins cannot reset another admin's password")
	wantCoded(t, h.call("PUT", "/api/admin/users/bob/quota", jsonBody(t, map[string]any{"bytes": 10485760, "confirm": amyPassword}), amy),
		http.StatusForbidden, "quota_admin_target", "admins cannot set a quota on an admin")
}

func TestChangePasswordErrorsCarryACode(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	wantCoded(t, h.do("PUT", "/api/me/password", jsonBody(t, map[string]any{"current": "not mine", "password": newPassword}), "192.0.2.10", amy),
		http.StatusUnauthorized, "wrong_current_password", "your current password is wrong")
	wantCoded(t, h.do("PUT", "/api/me/password", jsonBody(t, map[string]any{"current": amyPassword, "password": "short"}), "192.0.2.10", amy),
		http.StatusUnprocessableEntity, "password_invalid", "password must be 12 to 128 characters on a single line")
}

func TestFileErrorsCarryACode(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	h.write(t, "amy", "docs/a.txt", "x")

	cases := []struct {
		name          string
		rec           func() *httptest.ResponseRecorder
		status        int
		code, english string
	}{
		{"missing file", func() *httptest.ResponseRecorder { return h.get(amy, "/api/files/download", "nope.txt") }, 404, "not_found", "not found"},
		{"path outside", func() *httptest.ResponseRecorder { return h.get(amy, "/api/files", "../bob") }, 400, "path_invalid", "invalid path"},
		{"folder exists", func() *httptest.ResponseRecorder {
			return h.do("POST", "/api/files/folder", `{"path":"docs"}`, "192.0.2.10", amy)
		}, 409, "already_exists", "something with that name already exists"},
		{"file over a folder", func() *httptest.ResponseRecorder { return h.upload(amy, "docs", "x") }, 409, "folder_exists", "a folder with that name already exists"},
		{"too large", func() *httptest.ResponseRecorder {
			return h.upload(amy, "big.bin", strings.Repeat("a", portalMaxUpload+1))
		}, 413, "upload_too_large", "the file is larger than the upload limit"},
		{"rename onto existing", func() *httptest.ResponseRecorder {
			h.write(t, "amy", "b.txt", "y")
			return h.do("POST", "/api/files/rename", `{"from":"b.txt","to":"docs/a.txt"}`, "192.0.2.10", amy)
		}, 409, "already_exists", "something with that name already exists"},
		{"no preview", func() *httptest.ResponseRecorder {
			h.write(t, "amy", "odd.xyz", "z")
			return h.get(amy, "/api/files/view", "odd.xyz")
		}, 415, "preview_unsupported", "this file type can not be previewed"},
		{"upload session gone", func() *httptest.ResponseRecorder {
			return h.do("GET", "/api/uploads/"+url.PathEscape("0000"), "", "192.0.2.10", amy)
		}, 404, "not_found", "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantCoded(t, c.rec(), c.status, c.code, c.english)
		})
	}
}

func TestOverQuotaCarriesACode(t *testing.T) {
	h := newOverQuotaHarness(t, 10)
	rec := h.simpleUpload("big.txt", "12345678901234")
	wantCoded(t, rec, http.StatusInsufficientStorage, "quota_exceeded", overQuotaMessage)
}

func TestAFullDiskKeepsItsFlagAndGainsACode(t *testing.T) {
	h := newStorageHarness(t, 9900)
	rec := h.createUpload(h.amy, "new.bin", 10)
	wantCoded(t, rec, http.StatusInsufficientStorage, "disk_full", diskFullMessage)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || raw["diskFull"] != true {
		t.Fatalf("diskFull must stay: %s", rec.Body)
	}
}

func TestChunkErrorsKeepTheirOffsetAndGainACode(t *testing.T) {
	h := newOverQuotaHarness(t, 1<<20)
	rec := h.do("POST", "/api/uploads", `{"path":"a.bin","size":12}`, "192.0.2.10", h.cookie)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec = h.put("a.bin", "/api/uploads/"+created.ID, "56789012", map[string]string{"Content-Range": "bytes 4-11/12"})
	wantCoded(t, rec, http.StatusConflict, "upload_out_of_sync", "the chunk does not start where the server left off")
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || raw["offset"] != float64(0) {
		t.Fatalf("offset must stay: %s", rec.Body)
	}
}

func TestEmailFlowErrorsAndAnswersCarryACode(t *testing.T) {
	h := newPortalHarness(t)

	rec := h.call("POST", "/api/reset", jsonBody(t, map[string]any{"token": "nope", "password": newPassword}), nil)
	wantCoded(t, rec, http.StatusBadRequest, "link_invalid", "this link is not valid or has expired")
	rec = h.call("POST", "/api/email/confirm", jsonBody(t, map[string]any{"token": "nope"}), nil)
	wantCoded(t, rec, http.StatusBadRequest, "link_invalid", "this link is not valid or has expired")
	rec = h.call("POST", "/api/sign-in", jsonBody(t, map[string]any{"token": "nope"}), nil)
	wantCoded(t, rec, http.StatusBadRequest, "link_invalid", "this link is not valid or has expired")

	var answer struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	rec = h.call("POST", "/api/forgot", `{"username":"amy"}`, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK || answer.Code != "reset_link_sent" || answer.Message != forgotAnswer {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec = h.call("POST", "/api/sign-in-link", `{"email":"amy@example.com"}`, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK || answer.Code != "sign_in_link_sent" || answer.Message != signInAnswer {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestCodesNeverTouchTheFieldsAClientAlreadyReads(t *testing.T) {
	h := newPortalHarness(t)
	rec := h.login("amy", "not the password", "192.0.2.10")
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw) != 2 || raw["error"] == nil || raw["code"] == nil {
		t.Fatalf("an error body is exactly error and code: %s", rec.Body)
	}
}
