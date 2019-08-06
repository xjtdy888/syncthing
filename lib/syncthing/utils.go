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
	"io/ioutil"
	"os"

	"github.com/pkg/errors"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

func LoadOrGenerateCertificate(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(
		locations.Get(locations.CertFile),
		locations.Get(locations.KeyFile),
	)
	if err != nil {
		l.Infof("Generating ECDSA key and certificate for %s...", tlsDefaultCommonName)
		return tlsutil.NewCertificate(
			locations.Get(locations.CertFile),
			locations.Get(locations.KeyFile),
			tlsDefaultCommonName,
		)
	}
	return cert, nil
}

func DefaultConfig(path string, myID protocol.DeviceID, noDefaultFolder bool) (config.Wrapper, error) {
	newCfg, err := config.NewWithFreePorts(myID)
	if err != nil {
		return nil, err
	}

	if noDefaultFolder {
		l.Infoln("We will skip creation of a default folder on first start")
		return config.Wrap(path, newCfg), nil
	}

	newCfg.Folders = append(newCfg.Folders, config.NewFolderConfiguration(myID, "default", "Default Folder", fs.FilesystemTypeBasic, locations.Get(locations.DefFolder)))
	l.Infoln("Default folder created and/or linked to new config")
	return config.Wrap(path, newCfg), nil
}

// LoadConfigAtStartup loads an existing config. If it doesn't yet exist, it
// creates a default one, without the default folder if noDefaultFolder is ture.
// Otherwise it checks the version, and archives and upgrades the config if
// necessary or returns an error, if the version isn't compatible.
func LoadConfigAtStartup(path string, cert tls.Certificate, allowNewerConfig, noDefaultFolder bool) (config.Wrapper, error) {
	myID := protocol.NewDeviceID(cert.Certificate[0])
	cfg, err := config.Load(path, myID)
	if fs.IsNotExist(err) {
		cfg, err = DefaultConfig(path, myID, noDefaultFolder)
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate default config")
		}
		err = cfg.Save()
		if err != nil {
			return nil, errors.Wrap(err, "failed to save default config")
		}
		l.Infof("Default config saved. Edit %s to taste (with Syncthing stopped) or use the GUI", cfg.ConfigPath())
	} else if err == io.EOF {
		return nil, errors.New("failed to load config: unexpected end of file. Truncated or empty configuration?")
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to load config")
	}

	if cfg.RawCopy().OriginalVersion != config.CurrentVersion {
		if cfg.RawCopy().OriginalVersion == config.CurrentVersion+1101 {
			l.Infof("Now, THAT's what we call a config from the future! Don't worry. As long as you hit that wire with the connecting hook at precisely eighty-eight miles per hour the instant the lightning strikes the tower... everything will be fine.")
		}
		if cfg.RawCopy().OriginalVersion > config.CurrentVersion && !allowNewerConfig {
			return nil, fmt.Errorf("config file version (%d) is newer than supported version (%d). If this is expected, use -allow-newer-config to override.", cfg.RawCopy().OriginalVersion, config.CurrentVersion)
		}
		err = archiveAndSaveConfig(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "config archive")
		}
	}

	return cfg, nil
}

func archiveAndSaveConfig(cfg config.Wrapper) error {
	// Copy the existing config to an archive copy
	archivePath := cfg.ConfigPath() + fmt.Sprintf(".v%d", cfg.RawCopy().OriginalVersion)
	l.Infoln("Archiving a copy of old config file format at:", archivePath)
	if err := copyFile(cfg.ConfigPath(), archivePath); err != nil {
		return err
	}

	// Do a regular atomic config sve
	return cfg.Save()
}

func copyFile(src, dst string) error {
	bs, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(dst, bs, 0600); err != nil {
		// Attempt to clean up
		os.Remove(dst)
		return err
	}

	return nil
}

func OpenGoleveldb(path string) (*db.Lowlevel, error) {
	return db.Open(path)
}
