package server

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	defaultLogLines = 100
	maxFilterRunes  = 100
	maxLineDigits   = 4
)

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	s.protectedPage(w, r, "logs.html")
}

func (s *Server) readLogs(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	source, ok := system.ParseLogSource(query.Get("source"))
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "source must be sshd or panel")
		return
	}
	lines, ok := logLineCount(query)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "lines must be a whole number from 1 to 500")
		return
	}
	filter := query.Get("filter")
	if !validFilter(filter) {
		writeError(w, http.StatusUnprocessableEntity, "filter must be plain text of at most 100 characters")
		return
	}

	result, err := s.logs.Read(source, lines, filter)
	switch {
	case errors.Is(err, system.ErrInvalidLogRequest):
		writeError(w, http.StatusUnprocessableEntity, "invalid log request")
	case err != nil:
		log.Printf("read %s logs: %v", source, err)
		writeError(w, http.StatusInternalServerError, "could not read the logs")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"lines": result})
	}
}

// Digits only, so signs, spaces, exponents and other numerals never reach the parser.
func logLineCount(query url.Values) (int, bool) {
	if !query.Has("lines") {
		return defaultLogLines, true
	}
	value := query.Get("lines")
	if value == "" || len(value) > maxLineDigits {
		return 0, false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	lines, err := strconv.Atoi(value)

	return lines, err == nil && lines >= 1 && lines <= system.MaxLogLines
}

func validFilter(filter string) bool {
	if !utf8.ValidString(filter) || utf8.RuneCountInString(filter) > maxFilterRunes {
		return false
	}
	for _, r := range filter {
		if unicode.IsControl(r) {
			return false
		}
	}

	return true
}
