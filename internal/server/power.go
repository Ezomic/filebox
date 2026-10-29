package server

import (
	"log"
	"net/http"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

type powerRequest struct {
	Confirm bool `json:"confirm"`
}

func (s *Server) powerAction(action system.PowerAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body powerRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		if !body.Confirm {
			writeError(w, http.StatusUnprocessableEntity, "confirmation required")
			return
		}

		if action == system.RestartSSHD {
			s.restartSSHD(w)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		time.AfterFunc(s.powerDelay, func() {
			if err := s.power.Do(action); err != nil {
				log.Printf("power %s: %v", action, err)
			}
		})
	}
}

func (s *Server) restartSSHD(w http.ResponseWriter) {
	if err := s.power.Do(system.RestartSSHD); err != nil {
		log.Printf("power %s: %v", system.RestartSSHD, err)
		writeError(w, http.StatusInternalServerError, "could not restart sshd")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
