package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	placeholderPattern = regexp.MustCompile(`\{(\w+)\}`)
	keyPattern         = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-zA-Z][a-zA-Z0-9_]*)+$`)
	htmlKeyPattern     = regexp.MustCompile(`data-i18n="([^"]+)"`)
	htmlAttrPattern    = regexp.MustCompile(`data-i18n-attr="([^"]+)"`)
	literalPattern     = regexp.MustCompile(`'([a-z][a-zA-Z0-9]*(?:\.[a-zA-Z][a-zA-Z0-9_]*)+)'`)
	codePattern        = regexp.MustCompile(`= "([a-z_]+)"`)
)

// Single quoted strings that look like keys but are a storage key or a CSS selector.
var notTableKeys = map[string]bool{"photos.type": true, "nav.actions": true, "header.bar": true}

func loadTable(t *testing.T, lang string) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(webFiles, "web/"+lang+".json")
	if err != nil {
		t.Fatal(err)
	}
	table := map[string]string{}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("%s.json must be one flat object of strings: %v", lang, err)
	}

	return table
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

func placeholders(text string) []string {
	var found []string
	for _, match := range placeholderPattern.FindAllStringSubmatch(text, -1) {
		found = append(found, match[1])
	}
	sort.Strings(found)

	return found
}

func TestTablesHaveTheSameKeys(t *testing.T) {
	english, dutch := loadTable(t, "en"), loadTable(t, "nl")
	for _, key := range sortedKeys(english) {
		if _, ok := dutch[key]; !ok {
			t.Errorf("nl.json lacks %q", key)
		}
	}
	for _, key := range sortedKeys(dutch) {
		if _, ok := english[key]; !ok {
			t.Errorf("en.json lacks %q", key)
		}
	}
}

func TestTablePlaceholdersMatchBetweenLanguages(t *testing.T) {
	english, dutch := loadTable(t, "en"), loadTable(t, "nl")
	for _, key := range sortedKeys(english) {
		want, got := placeholders(english[key]), placeholders(dutch[key])
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("%q: en has %v, nl has %v", key, want, got)
		}
	}
}

func TestTableStringsAreCleanText(t *testing.T) {
	for _, lang := range []string{"en", "nl"} {
		table := loadTable(t, lang)
		for _, key := range sortedKeys(table) {
			value := table[key]
			switch {
			case !keyPattern.MatchString(key):
				t.Errorf("%s.json: key %q is not dotted camelCase words", lang, key)
			case strings.TrimSpace(value) == "":
				t.Errorf("%s.json: %q is empty", lang, key)
			case strings.ContainsRune(value, rune(0x2014)) || strings.ContainsRune(value, rune(0x2013)) || strings.Contains(value, "--"):
				t.Errorf("%s.json: %q contains a dash used as punctuation: %q", lang, key, value)
			case strings.ContainsAny(value, "<>"):
				t.Errorf("%s.json: %q contains markup, the pages insert text only: %q", lang, key, value)
			}
		}
	}
}

func TestPluralFormsComeInPairsWithTheCount(t *testing.T) {
	for _, lang := range []string{"en", "nl"} {
		table := loadTable(t, lang)
		for _, key := range sortedKeys(table) {
			base, ok := strings.CutSuffix(key, ".one")
			if !ok {
				continue
			}
			other, found := table[base+".other"]
			if !found {
				t.Errorf("%s.json: %q has no .other form", lang, key)
				continue
			}
			if !strings.Contains(table[key], "{count}") || !strings.Contains(other, "{count}") {
				t.Errorf("%s.json: the plural forms of %q must show {count}", lang, base)
			}
		}
	}
}

func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	add := func(name string) {
		raw, err := fs.ReadFile(webFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	entries, err := fs.ReadDir(webFiles, "web/portal")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		add("web/portal/" + entry.Name())
	}
	for _, name := range []string{"files.js", "trash.js", "photos.js", "viewer.js", "storage.js", "i18n.js"} {
		add("web/" + name)
	}

	return files
}

func usedKeys(t *testing.T) map[string]string {
	t.Helper()
	used := map[string]string{}
	for name, text := range sourceFiles(t) {
		if strings.HasSuffix(name, ".html") {
			for _, match := range htmlKeyPattern.FindAllStringSubmatch(text, -1) {
				used[match[1]] = name
			}
			for _, match := range htmlAttrPattern.FindAllStringSubmatch(text, -1) {
				for _, pair := range strings.Split(match[1], ",") {
					_, key, ok := strings.Cut(pair, ":")
					if !ok {
						t.Errorf("%s: data-i18n-attr %q needs attribute:key pairs", name, match[1])
					}
					used[key] = name
				}
			}
			continue
		}
		for _, match := range literalPattern.FindAllStringSubmatch(text, -1) {
			if !notTableKeys[match[1]] {
				used[match[1]] = name
			}
		}
	}

	return used
}

func hasKey(table map[string]string, key string) bool {
	if _, ok := table[key]; ok {
		return true
	}
	_, ok := table[key+".other"]

	return ok
}

func TestEveryKeyUsedByAPageExistsInEnglish(t *testing.T) {
	english := loadTable(t, "en")
	namespaces := map[string]bool{}
	for key := range english {
		namespaces[strings.SplitN(key, ".", 2)[0]] = true
	}
	for key, where := range usedKeys(t) {
		if strings.HasSuffix(where, ".js") && !namespaces[strings.SplitN(key, ".", 2)[0]] {
			continue
		}
		if !hasKey(english, key) {
			t.Errorf("%s uses %q, which en.json does not have", where, key)
		}
	}
}

func TestEveryTableKeyIsUsedSomewhere(t *testing.T) {
	used := usedKeys(t)
	codes := errorCodes(t)
	for _, key := range sortedKeys(loadTable(t, "en")) {
		base := strings.TrimSuffix(strings.TrimSuffix(key, ".one"), ".other")
		if _, ok := used[base]; ok {
			continue
		}
		if prefix, code, ok := strings.Cut(key, "."); ok && (prefix == "error" || prefix == "answer") && codes[code] {
			continue
		}
		t.Errorf("en.json key %q is not used by any page", key)
	}
}

func codeKey(code string) string {
	if strings.HasSuffix(code, "_sent") {
		return "answer." + code
	}

	return "error." + code
}

func errorCodes(t *testing.T) map[string]bool {
	t.Helper()
	source, err := os.ReadFile("codes.go")
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, match := range codePattern.FindAllStringSubmatch(string(source), -1) {
		codes[match[1]] = true
	}
	if len(codes) == 0 {
		t.Fatal("no error codes found in codes.go")
	}

	return codes
}

func TestEveryErrorCodeHasATranslationInBothLanguages(t *testing.T) {
	codes := errorCodes(t)
	for _, lang := range []string{"en", "nl"} {
		table := loadTable(t, lang)
		for _, code := range sortedKeys(codes) {
			if _, ok := table[codeKey(code)]; !ok {
				t.Errorf("%s.json lacks %s", lang, codeKey(code))
			}
		}
		for _, key := range sortedKeys(table) {
			prefix, code, ok := strings.Cut(key, ".")
			if ok && (prefix == "error" || prefix == "answer") && !codes[code] && code != "generic" && code != "network" {
				t.Errorf("%s.json: %q is not a code the server sends", lang, key)
			}
		}
	}
}

func TestEveryPortalPageLoadsTheTranslatorFirst(t *testing.T) {
	entries, err := fs.ReadDir(webFiles, "web/portal")
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, entry := range entries {
		if path.Ext(entry.Name()) != ".html" {
			continue
		}
		pages++
		raw, err := fs.ReadFile(webFiles, "web/portal/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		head := page[:strings.Index(page, "</head>")]
		if !strings.Contains(head, `<script src="/static/i18n.js"></script>`) {
			t.Errorf("%s must load i18n.js in the head so nothing paints untranslated", entry.Name())
		}
		if first := strings.Index(page, "<script"); first < 0 || !strings.HasPrefix(page[first:], `<script src="/static/i18n.js"`) {
			t.Errorf("%s must load i18n.js before any other script", entry.Name())
		}
	}
	if pages == 0 {
		t.Fatal("no portal pages found")
	}
}

func TestPortalServesTheTranslatorAndBothTables(t *testing.T) {
	h := newPortalHarness(t)
	cases := map[string]string{
		"/static/i18n.js": "javascript",
		"/static/nl.json": "application/json",
		"/static/en.json": "application/json",
	}
	for route, kind := range cases {
		rec := h.do("GET", route, "", "192.0.2.10", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), kind) {
			t.Errorf("%s got %d %q", route, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s must be served with nosniff", route)
		}
	}
	for _, route := range []string{"/static/fr.json", "/static/nl.json/", "/static/portal/nl.json", "/static/../nl.json"} {
		if rec := h.do("GET", route, "", "192.0.2.10", nil); rec.Code == http.StatusOK {
			t.Errorf("%s must not be served", route)
		}
	}
}

func TestAdminPanelDoesNotServeThePortalTables(t *testing.T) {
	h := newHarness(t)
	for _, route := range []string{"/static/i18n.js", "/static/nl.json", "/static/en.json"} {
		if rec := h.do("GET", route, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s got %d on the admin panel", route, rec.Code)
		}
	}
}
