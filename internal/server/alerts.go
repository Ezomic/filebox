package server

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/alerts"
)

type problemView struct {
	Subject string    `json:"subject"`
	Detail  string    `json:"detail"`
	Started time.Time `json:"started"`
}

type alertsView struct {
	// Checked is null until the first check after the panel started.
	Checked  *time.Time    `json:"checked"`
	Problems []problemView `json:"problems"`
	Address  string        `json:"address"`
	// Mail is false when mail.json is missing, so no alert email can go out.
	Mail bool `json:"mail"`
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.protectedPage(w, r, "settings.html")
}

func (s *Server) alertStatus(w http.ResponseWriter, _ *http.Request) {
	view := alertsView{Problems: []problemView{}}
	if s.alerts == nil {
		writeJSON(w, http.StatusOK, view)
		return
	}
	status := s.alerts.Status()
	view.Checked, view.Address, view.Mail = optionalTime(status.Checked), status.Address, status.Mail
	for _, alert := range status.Open {
		view.Problems = append(view.Problems, problemView{Subject: alert.Subject, Detail: alert.Detail, Started: alert.Started})
	}
	writeJSON(w, http.StatusOK, view)
}

type alertAddressRequest struct {
	Address string `json:"address"`
}

func (s *Server) setAlertAddress(w http.ResponseWriter, r *http.Request) {
	var body alertAddressRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if s.alerts == nil {
		writeError(w, http.StatusServiceUnavailable, "alerts are not available")
		return
	}
	address := strings.TrimSpace(body.Address)
	if address != "" && !validEmail(address) {
		writeError(w, http.StatusUnprocessableEntity, "enter one plain email address such as name@example.com")
		return
	}
	if err := s.alerts.SetAddress(address); err != nil {
		log.Printf("save alert address: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the address")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendTestAlert(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		writeError(w, http.StatusServiceUnavailable, "alerts are not available")
		return
	}
	switch err := s.alerts.SendTest(r.Context()); {
	case errors.Is(err, alerts.ErrMailOff):
		writeError(w, http.StatusConflict, "email is not set up on this box: mail.json is missing from the panel data folder")
	case errors.Is(err, alerts.ErrNoAddress):
		writeError(w, http.StatusConflict, "save an address first")
	case err != nil:
		log.Printf("test alert email not sent: %v", err)
		writeError(w, http.StatusBadGateway, "the mail service did not take the email, see the panel log")
	default:
		log.Print("test alert email sent")
		w.WriteHeader(http.StatusNoContent)
	}
}
