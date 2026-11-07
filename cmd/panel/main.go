package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Ezomic/filebox/internal/alerts"
	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/mail"
	"github.com/Ezomic/filebox/internal/server"
	"github.com/Ezomic/filebox/internal/system"
	"github.com/Ezomic/filebox/internal/tlsutil"
)

// Set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const alertEvery = 10 * time.Minute

func main() {
	listen := flag.String("listen", "127.0.0.1:8443", "address to listen on, normally the Tailscale IP")
	dataDir := flag.String("data", "./data", "directory for the admin password and TLS certificate")
	disk := flag.String("disk", "/", "path on the filesystem whose usage the dashboard shows, normally the SFTP storage")
	sans := flag.String("san", "localhost,127.0.0.1", "comma-separated hostnames and IPs for the self-signed certificate")
	filesDir := flag.String("files", "/srv/sftp", "directory holding each user's files folder, used by the portal and the housekeeping sweeps")
	maxUpload := flag.Int64("max-upload", 16<<30, "largest file the portal accepts, in bytes")
	portalListen := flag.String("portal-listen", "", "address for the public user portal, empty to disable it")
	portalCert := flag.String("portal-cert", "", "PEM certificate for the portal")
	portalKey := flag.String("portal-key", "", "PEM private key for the portal")
	streamboxURL := flag.String("streambox-url", "", "loopback URL of the streambox add-on, empty when it is not installed")
	streamboxEnv := flag.String("streambox-env", "/etc/streambox/streambox.env", "streambox settings file with its tokens")
	snapshotStatus := flag.String("snapshot-status", "/var/lib/filebox-snapshot/status", "status file filebox-snapshot writes for the earlier versions")
	healthStatus := flag.String("health-status", "/var/lib/filebox-health/status", "status file filebox-health writes with SMART and the btrfs data disk")
	backupStatus := flag.String("backup-status", "", "status file filebox-backup writes, empty when the backup add-on is not installed")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	log.Printf("filebox-panel %s", version)

	store, err := auth.Open(*dataDir)
	if err != nil {
		log.Fatalf("open admin store: %v", err)
	}
	if !store.Configured() {
		log.Printf("no admin yet: the setup token is in %s", filepath.Join(*dataDir, "setup-token"))
	}
	cert, err := tlsutil.LoadOrCreate(*dataDir, strings.Split(*sans, ","))
	if err != nil {
		log.Fatalf("prepare certificate: %v", err)
	}

	var tv *server.TV
	if *streamboxURL != "" {
		// A broken add-on must never take the admin panel down with it.
		if tv, err = server.NewTV(*streamboxURL, *streamboxEnv); err != nil {
			log.Printf("streambox add-on disabled: %v", err)
			tv = nil
		} else {
			log.Printf("streambox add-on enabled at %s", *streamboxURL)
		}
	}

	users := system.NewUsers()
	space := system.NewDiskSpace(*disk)
	thumbs := server.NewThumbs(system.NewThumbnailer())
	portalDeps := server.PortalDeps{
		Verifier:           system.NewVerifier(),
		Users:              users,
		Roles:              store,
		OpenFiles:          system.UserFilesOpener(*filesDir),
		OpenFamily:         system.FamilyFilesOpener(*filesDir),
		OpenVersions:       system.UserVersionsOpener(*filesDir),
		OpenFamilyVersions: system.FamilyVersionsOpener(*filesDir),
		MaxUpload:          *maxUpload,
		Disk:               space,
		TV:                 tv,
		Thumbs:             thumbs,
	}
	shares, err := auth.OpenShares(*dataDir)
	if err != nil {
		log.Fatalf("open share links: %v", err)
	}
	portalDeps.Shares = shares
	sender, portalURL := loadMail(*dataDir)
	enableEmail(&portalDeps, *dataDir, sender, portalURL)

	health := system.NewHealth(*healthStatus)
	snapshots := func() (system.SnapshotStatus, error) { return system.ReadSnapshotStatus(*snapshotStatus) }
	var backup func() (system.BackupStatus, error)
	if *backupStatus != "" {
		backup = func() (system.BackupStatus, error) { return system.ReadBackupStatus(*backupStatus) }
		log.Printf("backup add-on enabled, status in %s", *backupStatus)
	}
	var portalCerts *tlsutil.Reloader
	var certExpiry func() time.Time
	if *portalListen != "" {
		portalCerts, err = tlsutil.NewReloader(*portalCert, *portalKey)
		if err != nil {
			log.Fatalf("load portal certificate: %v", err)
		}
		certExpiry = portalCerts.NotAfter
	}
	monitor := openAlerts(*dataDir, sender, space, health, snapshots, backup, certExpiry)
	portalDeps.Healthy = monitor.Healthy
	portal := server.NewPortal(portalDeps)

	deps := server.Deps{
		Host:       system.NewHost(*disk),
		Users:      users,
		Logs:       system.NewLogs(),
		Power:      system.NewPower(),
		Usage:      system.NewUsage(*filesDir),
		Smart:      health,
		Disk:       space,
		PowerDelay: time.Second,

		OnUserChanged:    portal.EndSessions,
		PortalCertExpiry: certExpiry,
		Version:          version,
		TV:               tv,
		Snapshots:        snapshots,
		Alerts:           monitor,
		Offsite:          backup,
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.New(store, deps),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	servers := []*http.Server{srv}
	if *portalListen != "" {
		servers = append(servers, portalServer(*portalListen, portalCerts, portal))
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go system.RunUploadSweeper(stop, *filesDir, 24*time.Hour, time.Now)
	go system.RunTrashSweeper(stop, *filesDir, 24*time.Hour, time.Now)
	go system.RunThumbSweeper(stop, *filesDir, 24*time.Hour, time.Now)
	go monitor.Run(stop, alertEvery)
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

func portalServer(addr string, certs *tlsutil.Reloader, portal http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           portal,
		TLSConfig:         &tls.Config{GetCertificate: certs.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// Email stays off, and the panel still starts, unless mail.json holds a usable config. The sender is nil then.
func loadMail(dataDir string) (mail.Sender, string) {
	config, ok, err := mail.LoadConfig(filepath.Join(dataDir, "mail.json"))
	switch {
	case err != nil:
		log.Printf("email is off: mail.json: %v", err)
		return nil, ""
	case !ok:
		log.Print("email is off: no mail.json")
		return nil, ""
	}

	return mail.NewZero(config.URL, config.Token), config.PortalURL
}

func enableEmail(deps *server.PortalDeps, dataDir string, sender mail.Sender, portalURL string) {
	if sender == nil {
		log.Print("password reset by email is off")
		return
	}
	contacts, err := auth.OpenContacts(dataDir)
	if err != nil {
		log.Printf("password reset by email is off: open contacts: %v", err)
		return
	}
	deps.Contacts = contacts
	deps.Mail = sender
	deps.PortalURL = portalURL
	log.Print("password reset by email is on")
}

// Problems still show on the dashboard without mail.json; only the emails need it. backup is nil without the add-on.
func openAlerts(dataDir string, sender mail.Sender, space system.DiskSpace, health *system.Health,
	snapshots func() (system.SnapshotStatus, error), backup func() (system.BackupStatus, error), certExpiry func() time.Time) *alerts.Monitor {
	counters, err := alerts.OpenCounters(dataDir)
	if err != nil {
		log.Fatalf("open alert counters: %v", err)
	}
	failing, err := alerts.OpenFailingDisks(dataDir)
	if err != nil {
		log.Fatalf("open failing disks: %v", err)
	}
	checks := []alerts.Check{
		alerts.DiskCheck(space),
		alerts.SmartCheck(health, counters, failing),
		alerts.MirrorCheck(health.Btrfs, counters),
		alerts.SnapshotCheck(snapshots),
		alerts.CertCheck(certExpiry),
		alerts.RestartCheck(system.NewBoot()),
	}
	if backup != nil {
		checks = append(checks, alerts.BackupCheck(backup))
	}
	monitor, err := alerts.Open(dataDir, sender, checks)
	if err != nil {
		log.Fatalf("open alerts: %v", err)
	}

	return monitor
}
