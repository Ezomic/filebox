package server

import (
	"log"
	"net/http"

	"github.com/Ezomic/filebox/internal/system"
)

func (s *Server) disks(w http.ResponseWriter, _ *http.Request) {
	disks, err := s.smart.Disks()
	if err != nil {
		log.Printf("read disk health: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "disks": []system.DiskHealth{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "disks": disks})
}
