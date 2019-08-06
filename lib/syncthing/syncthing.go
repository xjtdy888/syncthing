// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package syncthing

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/thejerf/suture"

	"github.com/syncthing/syncthing/lib/api"
	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/connections"
	"github.com/syncthing/syncthing/lib/db"
	"github.com/syncthing/syncthing/lib/discover"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/logger"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/rand"
	"github.com/syncthing/syncthing/lib/sha256"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"github.com/syncthing/syncthing/lib/ur"
)

const (
	bepProtocolName      = "bep/1.0"
	tlsDefaultCommonName = "syncthing"
	maxSystemErrors      = 5
	initialSystemLog     = 10
	maxSystemLog         = 250
)

type ExitStatus int

const (
	ExitSuccess ExitStatus = 0
	ExitError   ExitStatus = 1
	ExitRestart ExitStatus = 3
	ExitUpgrade ExitStatus = 4
)

type Options struct {
	AssetDir         string
	AuditWriter      io.Writer
	DeadlockTimeoutS int
	NoUpgrade        bool
	ProfilerURL      string
	ResetDeltaIdxs   bool
	Verbose          bool
}

type App struct {
	myID        protocol.DeviceID
	mainService *suture.Supervisor
	cfg         config.Wrapper
	ll          *db.Lowlevel
	cert        tls.Certificate
	opts        Options
	exitStatus  ExitStatus
	err         error
	startOnce   sync.Once
	stopOnce    sync.Once
	stop        chan struct{}
	stopped     chan struct{}
}

func New(cfg config.Wrapper, ll *db.Lowlevel, cert tls.Certificate, opts Options) *App {
	return &App{
		cfg:     cfg,
		ll:      ll,
		opts:    opts,
		cert:    cert,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Run does the same as start, but then does not return until the app stops. It
// is equivalent to calling Start and then Wait.
func (a *App) Run() ExitStatus {
	a.Start()
	return a.Wait()
}

// Start executes the app and returns once all the startup operations are done,
// e.g. the API is ready for use.
func (a *App) Start() {
	a.startOnce.Do(func() {
		if err := a.startup(); err != nil {
			a.stopWithErr(ExitError, err)
			return
		}
		go a.run()
	})
}

func (a *App) startup() error {
	// Create a main service manager. We'll add things to this as we go along.
	// We want any logging it does to go through our log system.
	a.mainService = suture.New("main", suture.Spec{
		Log: func(line string) {
			l.Debugln(line)
		},
		PassThroughPanics: true,
	})
	a.mainService.ServeBackground()

	if a.opts.AuditWriter != nil {
		a.mainService.Add(newAuditService(a.opts.AuditWriter))
	}

	if a.opts.Verbose {
		a.mainService.Add(newVerboseService())
	}

	errors := logger.NewRecorder(l, logger.LevelWarn, maxSystemErrors, 0)
	systemLog := logger.NewRecorder(l, logger.LevelDebug, maxSystemLog, initialSystemLog)

	// Event subscription for the API; must start early to catch the early
	// events. The LocalChangeDetected event might overwhelm the event
	// receiver in some situations so we will not subscribe to it here.
	defaultSub := events.NewBufferedSubscription(events.Default.Subscribe(api.DefaultEventMask), api.EventSubBufferSize)
	diskSub := events.NewBufferedSubscription(events.Default.Subscribe(api.DiskEventMask), api.EventSubBufferSize)

	// Attempt to increase the limit on number of open files to the maximum
	// allowed, in case we have many peers. We don't really care enough to
	// report the error if there is one.
	osutil.MaximizeOpenFileLimit()

	// Figure out our device ID, set it as the log prefix and log it.
	a.myID = protocol.NewDeviceID(a.cert.Certificate[0])
	l.SetPrefix(fmt.Sprintf("[%s] ", a.myID.String()[:5]))
	l.Infoln("My ID:", a.myID)

	// Select SHA256 implementation and report. Affected by the
	// STHASHING environment variable.
	sha256.SelectAlgo()
	sha256.Report()

	// Emit the Starting event, now that we know who we are.

	events.Default.Log(events.Starting, map[string]string{
		"home": locations.GetBaseDir(locations.ConfigBaseDir),
		"myID": a.myID.String(),
	})

	if err := checkShortIDs(a.cfg); err != nil {
		l.Warnln("Short device IDs are in conflict. Unlucky!\n  Regenerate the device ID of one of the following:\n  ", err)
		return err
	}

	if len(a.opts.ProfilerURL) > 0 {
		go func() {
			l.Debugln("Starting profiler on", a.opts.ProfilerURL)
			runtime.SetBlockProfileRate(1)
			err := http.ListenAndServe(a.opts.ProfilerURL, nil)
			if err != nil {
				l.Warnln(err)
				return
			}
		}()
	}

	perf := ur.CpuBench(3, 150*time.Millisecond, true)
	l.Infof("Hashing performance is %.02f MB/s", perf)

	if err := db.UpdateSchema(a.ll); err != nil {
		l.Warnln("Database schema:", err)
		return err
	}

	if a.opts.ResetDeltaIdxs {
		l.Infoln("Reinitializing delta index IDs")
		db.DropDeltaIndexIDs(a.ll)
	}

	protectedFiles := []string{
		locations.Get(locations.Database),
		locations.Get(locations.ConfigFile),
		locations.Get(locations.CertFile),
		locations.Get(locations.KeyFile),
	}

	// Remove database entries for folders that no longer exist in the config
	folders := a.cfg.Folders()
	for _, folder := range a.ll.ListFolders() {
		if _, ok := folders[folder]; !ok {
			l.Infof("Cleaning data for dropped folder %q", folder)
			db.DropFolder(a.ll, folder)
		}
	}

	// Grab the previously running version string from the database.

	miscDB := db.NewMiscDataNamespace(a.ll)
	prevVersion, _ := miscDB.String("prevVersion")

	// Strip away prerelease/beta stuff and just compare the release
	// numbers. 0.14.44 to 0.14.45-banana is an upgrade, 0.14.45-banana to
	// 0.14.45-pineapple is not.

	prevParts := strings.Split(prevVersion, "-")
	curParts := strings.Split(build.Version, "-")
	if prevParts[0] != curParts[0] {
		if prevVersion != "" {
			l.Infoln("Detected upgrade from", prevVersion, "to", build.Version)
		}

		// Drop delta indexes in case we've changed random stuff we
		// shouldn't have. We will resend our index on next connect.
		db.DropDeltaIndexIDs(a.ll)

		// Remember the new version.
		miscDB.PutString("prevVersion", build.Version)
	}

	m := model.NewModel(a.cfg, a.myID, "syncthing", build.Version, a.ll, protectedFiles)

	if a.opts.DeadlockTimeoutS > 0 {
		m.StartDeadlockDetector(time.Duration(a.opts.DeadlockTimeoutS) * time.Second)
	} else if !build.IsRelease || build.IsBeta {
		m.StartDeadlockDetector(20 * time.Minute)
	}

	// Add and start folders
	for _, folderCfg := range a.cfg.Folders() {
		if folderCfg.Paused {
			folderCfg.CreateRoot()
			continue
		}
		m.AddFolder(folderCfg)
		m.StartFolder(folderCfg.ID)
	}

	a.mainService.Add(m)

	// Start discovery

	cachedDiscovery := discover.NewCachingMux()
	a.mainService.Add(cachedDiscovery)

	// The TLS configuration is used for both the listening socket and outgoing
	// connections.

	tlsCfg := tlsutil.SecureDefault()
	tlsCfg.Certificates = []tls.Certificate{a.cert}
	tlsCfg.NextProtos = []string{bepProtocolName}
	tlsCfg.ClientAuth = tls.RequestClientCert
	tlsCfg.SessionTicketsDisabled = true
	tlsCfg.InsecureSkipVerify = true

	// Start connection management

	connectionsService := connections.NewService(a.cfg, a.myID, m, tlsCfg, cachedDiscovery, bepProtocolName, tlsDefaultCommonName)
	a.mainService.Add(connectionsService)

	if a.cfg.Options().GlobalAnnEnabled {
		for _, srv := range a.cfg.GlobalDiscoveryServers() {
			l.Infoln("Using discovery server", srv)
			gd, err := discover.NewGlobal(srv, a.cert, connectionsService)
			if err != nil {
				l.Warnln("Global discovery:", err)
				continue
			}

			// Each global discovery server gets its results cached for five
			// minutes, and is not asked again for a minute when it's returned
			// unsuccessfully.
			cachedDiscovery.Add(gd, 5*time.Minute, time.Minute)
		}
	}

	if a.cfg.Options().LocalAnnEnabled {
		// v4 broadcasts
		bcd, err := discover.NewLocal(a.myID, fmt.Sprintf(":%d", a.cfg.Options().LocalAnnPort), connectionsService)
		if err != nil {
			l.Warnln("IPv4 local discovery:", err)
		} else {
			cachedDiscovery.Add(bcd, 0, 0)
		}
		// v6 multicasts
		mcd, err := discover.NewLocal(a.myID, a.cfg.Options().LocalAnnMCAddr, connectionsService)
		if err != nil {
			l.Warnln("IPv6 local discovery:", err)
		} else {
			cachedDiscovery.Add(mcd, 0, 0)
		}
	}

	// Candidate builds always run with usage reporting.

	if opts := a.cfg.Options(); build.IsCandidate {
		l.Infoln("Anonymous usage reporting is always enabled for candidate releases.")
		if opts.URAccepted != ur.Version {
			opts.URAccepted = ur.Version
			a.cfg.SetOptions(opts)
			a.cfg.Save()
			// Unique ID will be set and config saved below if necessary.
		}
	}

	// If we are going to do usage reporting, ensure we have a valid unique ID.
	if opts := a.cfg.Options(); opts.URAccepted > 0 && opts.URUniqueID == "" {
		opts.URUniqueID = rand.String(8)
		a.cfg.SetOptions(opts)
		a.cfg.Save()
	}

	usageReportingSvc := ur.New(a.cfg, m, connectionsService, a.opts.NoUpgrade)
	a.mainService.Add(usageReportingSvc)

	// GUI

	if err := a.setupGUI(m, defaultSub, diskSub, cachedDiscovery, connectionsService, usageReportingSvc, errors, systemLog); err != nil {
		l.Warnln("Failed starting API:", err)
		return err
	}

	myDev, _ := a.cfg.Device(a.myID)
	l.Infof(`My name is "%v"`, myDev.Name)
	for _, device := range a.cfg.Devices() {
		if device.DeviceID != a.myID {
			l.Infof(`Device %s is "%v" at %v`, device.DeviceID, device.Name, device.Addresses)
		}
	}

	if isSuperUser() {
		l.Warnln("Syncthing should not run as a privileged or system user. Please consider using a normal user account.")
	}

	events.Default.Log(events.StartupComplete, map[string]string{
		"myID": a.myID.String(),
	})

	if a.cfg.Options().SetLowPriority {
		if err := osutil.SetLowPriority(); err != nil {
			l.Warnln("Failed to lower process priority:", err)
		}
	}

	return nil
}

func (a *App) run() {
	<-a.stop

	a.mainService.Stop()

	done := make(chan struct{})
	go func() {
		a.ll.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		l.Warnln("Database failed to stop within 10s")
	}

	l.Infoln("Exiting")

	close(a.stopped)
}

// Wait blocks until the app stops running.
func (a *App) Wait() ExitStatus {
	<-a.stopped
	return a.exitStatus
}

// Error returns an error if one occurred while running the app. It does not wait
// for the app to stop before returning.
func (a *App) Error() error {
	select {
	case <-a.stopped:
		return nil
	default:
	}
	return a.err
}

// Stop stops the app and sets its exit status to given reason, unless the app
// was already stopped before. In any case it returns the effective exit status.
func (a *App) Stop(stopReason ExitStatus) ExitStatus {
	return a.stopWithErr(stopReason, nil)
}

func (a *App) stopWithErr(stopReason ExitStatus, err error) ExitStatus {
	a.stopOnce.Do(func() {
		// ExitSuccess is the default value for a.exitStatus. If another status
		// was already set, ignore the stop reason given as argument to Stop.
		if a.exitStatus == ExitSuccess {
			a.exitStatus = stopReason
			a.err = err
		}
		close(a.stop)
	})
	return a.exitStatus
}

func (a *App) setupGUI(m model.Model, defaultSub, diskSub events.BufferedSubscription, discoverer discover.CachingMux, connectionsService connections.Service, urService *ur.Service, errors, systemLog logger.Recorder) error {
	guiCfg := a.cfg.GUI()

	if !guiCfg.Enabled {
		return nil
	}

	if guiCfg.InsecureAdminAccess {
		l.Warnln("Insecure admin access is enabled.")
	}

	cpu := newCPUService()
	a.mainService.Add(cpu)

	summaryService := model.NewFolderSummaryService(a.cfg, m, a.myID)
	a.mainService.Add(summaryService)

	apiSvc := api.New(a.myID, a.cfg, a.opts.AssetDir, tlsDefaultCommonName, m, defaultSub, diskSub, discoverer, connectionsService, urService, summaryService, errors, systemLog, cpu, &controller{a}, a.opts.NoUpgrade)
	a.mainService.Add(apiSvc)

	if err := apiSvc.WaitForStart(); err != nil {
		return err
	}
	return nil
}

// checkShortIDs verifies that the configuration won't result in duplicate
// short ID:s; that is, that the devices in the cluster all have unique
// initial 64 bits.
func checkShortIDs(cfg config.Wrapper) error {
	exists := make(map[protocol.ShortID]protocol.DeviceID)
	for deviceID := range cfg.Devices() {
		shortID := deviceID.Short()
		if otherID, ok := exists[shortID]; ok {
			return fmt.Errorf("%v in conflict with %v", deviceID, otherID)
		}
		exists[shortID] = deviceID
	}
	return nil
}

// Implements api.Controller
type controller struct{ *App }

func (e *controller) Restart() {
	e.Stop(ExitRestart)
}

func (e *controller) Shutdown() {
	e.Stop(ExitSuccess)
}

func (e *controller) ExitUpgrading() {
	e.Stop(ExitUpgrade)
}
