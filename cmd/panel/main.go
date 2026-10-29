package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/server"
	"github.com/Ezomic/filebox/internal/system"
	"github.com/Ezomic/filebox/internal/tlsutil"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8443", "address to listen on, normally the Tailscale IP")
	dataDir := flag.String("data", "./data", "directory for the admin password and TLS certificate")
	disk := flag.String("disk", "/", "path on the filesystem whose usage the dashboard shows, normally the SFTP storage")
	sans := flag.String("san", "localhost,127.0.0.1", "comma-separated hostnames and IPs for the self-signed certificate")
	filesDir := flag.String("files", "/srv/sftp", "directory the file browser is confined to")
	maxUpload := flag.Int64("max-upload", 4<<30, "largest file the file browser accepts, in bytes")
	portalListen := flag.String("portal-listen", "", "address for the public user portal, empty to disable it")
	portalCert := flag.String("portal-cert", "", "PEM certificate for the portal")
	portalKey := flag.String("portal-key", "", "PEM private key for the portal")
	flag.Parse()

	store, err := auth.Open(*dataDir)
	if err != nil {
		log.Fatalf("open admin store: %v", err)
	}
	if !store.Configured() {
		log.Printf("no admin yet: the setup token is in %s", filepath.Join(*dataDir, "setup-token"))
	}
	files, err := system.NewFiles(*filesDir)
	if err != nil {
		log.Fatalf("open file root %s: %v", *filesDir, err)
	}
	cert, err := tlsutil.LoadOrCreate(*dataDir, strings.Split(*sans, ","))
	if err != nil {
		log.Fatalf("prepare certificate: %v", err)
	}

	users := system.NewUsers()
	portal := server.NewPortal(server.PortalDeps{
		Verifier:  system.NewVerifier(),
		Users:     users,
		OpenFiles: system.UserFilesOpener(*filesDir),
		MaxUpload: *maxUpload,
	})

	srv := &http.Server{
		Addr: *listen,
		Handler: server.New(store, server.Deps{
			Host:       system.NewHost(*disk),
			Users:      users,
			Files:      files,
			Logs:       system.NewLogs(),
			Power:      system.NewPower(),
			Usage:      system.NewUsage(*filesDir),
			Smart:      system.NewSmart(),
			PowerDelay: time.Second,
			MaxUpload:  *maxUpload,

			OnUserChanged: portal.EndSessions,
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	servers := []*http.Server{srv}
	if *portalListen != "" {
		servers = append(servers, portalServer(*portalListen, *portalCert, *portalKey, portal))
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	failed := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Printf("listening on https://%s", s.Addr)
			failed <- s.ListenAndServeTLS("", "")
		}()
	}

	select {
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-stop.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
}

func portalServer(addr, certFile, keyFile string, portal http.Handler) *http.Server {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("load portal certificate: %v", err)
	}

	return &http.Server{
		Addr:              addr,
		Handler:           portal,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}
