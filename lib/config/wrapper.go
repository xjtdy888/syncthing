// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/rand"
	"github.com/syncthing/syncthing/lib/sync"
	"github.com/syncthing/syncthing/lib/util"
)

// The Committer interface is implemented by objects that need to know about
// or have a say in configuration changes.
//
// When the configuration is about to be changed, VerifyConfiguration() is
// called for each subscribing object, with the old and new configuration. A
// nil error is returned if the new configuration is acceptable (i.e. does not
// contain any errors that would prevent it from being a valid config).
// Otherwise an error describing the problem is returned.
//
// If any subscriber returns an error from VerifyConfiguration(), the
// configuration change is not committed and an error is returned to whoever
// tried to commit the broken config.
//
// If all verification calls returns nil, CommitConfiguration() is called for
// each subscribing object. The callee returns true if the new configuration
// has been successfully applied, otherwise false. Any Commit() call returning
// false will result in a "restart needed" response to the API/user. Note that
// the new configuration will still have been applied by those who were
// capable of doing so.
type Committer interface {
	VerifyConfiguration(from, to Configuration) error
	CommitConfiguration(from, to Configuration) (handled bool)
	String() string
}

// Waiter allows to wait for the given config operation to complete.
type Waiter interface {
	Wait()
}

type noopWaiter struct{}

func (noopWaiter) Wait() {}

// A Wrapper around a Configuration that manages loads, saves and published
// notifications of changes to registered Handlers
type Wrapper interface {
	MyName() string
	ConfigPath() string

	RawCopy() Configuration
	Replace(cfg Configuration) (Waiter, error)
	RequiresRestart() bool
	Save() error

	GUI() GUIConfiguration
	SetGUI(gui GUIConfiguration) (Waiter, error)
	LDAP() LDAPConfiguration

	Options() OptionsConfiguration
	SetOptions(opts OptionsConfiguration) (Waiter, error)

	Folder(id string) (FolderConfiguration, bool)
	Folders() map[string]FolderConfiguration
	FolderList() []FolderConfiguration
	SetFolder(fld FolderConfiguration) (Waiter, error)

	Device(id protocol.DeviceID) (DeviceConfiguration, bool)
	Devices() map[protocol.DeviceID]DeviceConfiguration
	RemoveDevice(id protocol.DeviceID) (Waiter, error)
	SetDevice(DeviceConfiguration) (Waiter, error)
	SetDevices([]DeviceConfiguration) (Waiter, error)

	AddOrUpdatePendingDevice(device protocol.DeviceID, name, address string)
	AddOrUpdatePendingFolder(id, label string, device protocol.DeviceID)
	IgnoredDevice(id protocol.DeviceID) bool
	IgnoredFolder(device protocol.DeviceID, folder string) bool

	ListenAddresses() []string
	GlobalDiscoveryServers() []string
	StunServers() []string

	Subscribe(c Committer)
	Unsubscribe(c Committer)
}

type wrapper struct {
	cfg  Configuration
	path string

	deviceMap map[protocol.DeviceID]DeviceConfiguration
	folderMap map[string]FolderConfiguration
	subs      []Committer
	mut       sync.Mutex

	requiresRestart uint32 // an atomic bool
}

func (w *wrapper) StunServers() []string {
	var addresses []string
	for _, addr := range w.cfg.Options.StunServers {
		switch addr {
		case "default":
			defaultPrimaryAddresses := make([]string, len(DefaultPrimaryStunServers))
			copy(defaultPrimaryAddresses, DefaultPrimaryStunServers)
			rand.Shuffle(defaultPrimaryAddresses)
			addresses = append(addresses, defaultPrimaryAddresses...)

			defaultSecondaryAddresses := make([]string, len(DefaultSecondaryStunServers))
			copy(defaultSecondaryAddresses, DefaultSecondaryStunServers)
			rand.Shuffle(defaultSecondaryAddresses)
			addresses = append(addresses, defaultSecondaryAddresses...)
		default:
			addresses = append(addresses, addr)
		}
	}

	addresses = util.UniqueTrimmedStrings(addresses)

	return addresses
}

// Wrap wraps an existing Configuration structure and ties it to a file on
// disk.
func Wrap(path string, cfg Configuration) Wrapper {
	w := &wrapper{
		cfg:  cfg,
		path: path,
		mut:  sync.NewMutex(),
	}
	return w
}

// Load loads an existing file on disk and returns a new configuration
// wrapper.
func Load(path string, myID protocol.DeviceID) (Wrapper, error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	cfg, err := ReadXML(fd, myID)
	if err != nil {
		return nil, err
	}

	return Wrap(path, cfg), nil
}

func (w *wrapper) ConfigPath() string {
	return w.path
}

// Subscribe registers the given handler to be called on any future
// configuration changes.
func (w *wrapper) Subscribe(c Committer) {
	w.mut.Lock()
	w.subs = append(w.subs, c)
	w.mut.Unlock()
}

// Unsubscribe de-registers the given handler from any future calls to
// configuration changes
func (w *wrapper) Unsubscribe(c Committer) {
	w.mut.Lock()
	for i := range w.subs {
		if w.subs[i] == c {
			copy(w.subs[i:], w.subs[i+1:])
			w.subs[len(w.subs)-1] = nil
			w.subs = w.subs[:len(w.subs)-1]
			break
		}
	}
	w.mut.Unlock()
}

// RawCopy returns a copy of the currently wrapped Configuration object.
func (w *wrapper) RawCopy() Configuration {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.cfg.Copy()
}

// Replace swaps the current configuration object for the given one.
func (w *wrapper) Replace(cfg Configuration) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.replaceLocked(cfg.Copy())
}

func (w *wrapper) replaceLocked(to Configuration) (Waiter, error) {
	from := w.cfg

	if err := to.clean(); err != nil {
		return noopWaiter{}, err
	}

	for _, sub := range w.subs {
		l.Debugln(sub, "verifying configuration")
		if err := sub.VerifyConfiguration(from.Copy(), to.Copy()); err != nil {
			l.Debugln(sub, "rejected config:", err)
			return noopWaiter{}, err
		}
	}

	w.cfg = to
	w.deviceMap = nil
	w.folderMap = nil

	return w.notifyListeners(from.Copy(), to.Copy()), nil
}

func (w *wrapper) notifyListeners(from, to Configuration) Waiter {
	wg := sync.NewWaitGroup()
	wg.Add(len(w.subs))
	for _, sub := range w.subs {
		go func(commiter Committer) {
			w.notifyListener(commiter, from, to)
			wg.Done()
		}(sub)
	}
	return wg
}

func (w *wrapper) notifyListener(sub Committer, from, to Configuration) {
	l.Debugln(sub, "committing configuration")
	if !sub.CommitConfiguration(from, to) {
		l.Debugln(sub, "requires restart")
		w.setRequiresRestart()
	}
}

// Devices returns a map of devices.
func (w *wrapper) Devices() map[protocol.DeviceID]DeviceConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	if w.deviceMap == nil {
		w.deviceMap = make(map[protocol.DeviceID]DeviceConfiguration, len(w.cfg.Devices))
		for _, dev := range w.cfg.Devices {
			w.deviceMap[dev.DeviceID] = dev.Copy()
		}
	}
	return w.deviceMap
}

// SetDevices adds new devices to the configuration, or overwrites existing
// devices with the same ID.
func (w *wrapper) SetDevices(devs []DeviceConfiguration) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()

	newCfg := w.cfg.Copy()
	var replaced bool
	for oldIndex := range devs {
		replaced = false
		for newIndex := range newCfg.Devices {
			if newCfg.Devices[newIndex].DeviceID == devs[oldIndex].DeviceID {
				newCfg.Devices[newIndex] = devs[oldIndex].Copy()
				replaced = true
				break
			}
		}
		if !replaced {
			newCfg.Devices = append(newCfg.Devices, devs[oldIndex].Copy())
		}
	}

	return w.replaceLocked(newCfg)
}

// SetDevice adds a new device to the configuration, or overwrites an existing
// device with the same ID.
func (w *wrapper) SetDevice(dev DeviceConfiguration) (Waiter, error) {
	return w.SetDevices([]DeviceConfiguration{dev})
}

// RemoveDevice removes the device from the configuration
func (w *wrapper) RemoveDevice(id protocol.DeviceID) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()

	newCfg := w.cfg.Copy()
	for i := range newCfg.Devices {
		if newCfg.Devices[i].DeviceID == id {
			newCfg.Devices = append(newCfg.Devices[:i], newCfg.Devices[i+1:]...)
			return w.replaceLocked(newCfg)
		}
	}

	return noopWaiter{}, nil
}

// Folders returns a map of folders. Folder structures should not be changed,
// other than for the purpose of updating via SetFolder().
func (w *wrapper) Folders() map[string]FolderConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	if w.folderMap == nil {
		w.folderMap = make(map[string]FolderConfiguration, len(w.cfg.Folders))
		for _, fld := range w.cfg.Folders {
			w.folderMap[fld.ID] = fld.Copy()
		}
	}
	return w.folderMap
}

// FolderList returns a slice of folders.
func (w *wrapper) FolderList() []FolderConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.cfg.Copy().Folders
}

// SetFolder adds a new folder to the configuration, or overwrites an existing
// folder with the same ID.
func (w *wrapper) SetFolder(fld FolderConfiguration) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()

	newCfg := w.cfg.Copy()

	for i := range newCfg.Folders {
		if newCfg.Folders[i].ID == fld.ID {
			newCfg.Folders[i] = fld
			return w.replaceLocked(newCfg)
		}
	}

	newCfg.Folders = append(newCfg.Folders, fld)

	return w.replaceLocked(newCfg)
}

// Options returns the current options configuration object.
func (w *wrapper) Options() OptionsConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.cfg.Options.Copy()
}

// SetOptions replaces the current options configuration object.
func (w *wrapper) SetOptions(opts OptionsConfiguration) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()
	newCfg := w.cfg.Copy()
	newCfg.Options = opts.Copy()
	return w.replaceLocked(newCfg)
}

func (w *wrapper) LDAP() LDAPConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.cfg.LDAP.Copy()
}

// GUI returns the current GUI configuration object.
func (w *wrapper) GUI() GUIConfiguration {
	w.mut.Lock()
	defer w.mut.Unlock()
	return w.cfg.GUI.Copy()
}

// SetGUI replaces the current GUI configuration object.
func (w *wrapper) SetGUI(gui GUIConfiguration) (Waiter, error) {
	w.mut.Lock()
	defer w.mut.Unlock()
	newCfg := w.cfg.Copy()
	newCfg.GUI = gui.Copy()
	return w.replaceLocked(newCfg)
}

// IgnoredDevice returns whether or not connection attempts from the given
// device should be silently ignored.
func (w *wrapper) IgnoredDevice(id protocol.DeviceID) bool {
	w.mut.Lock()
	defer w.mut.Unlock()
	for _, device := range w.cfg.IgnoredDevices {
		if device.ID == id {
			return true
		}
	}
	return false
}

// IgnoredFolder returns whether or not share attempts for the given
// folder should be silently ignored.
func (w *wrapper) IgnoredFolder(device protocol.DeviceID, folder string) bool {
	dev, ok := w.Device(device)
	if !ok {
		return false
	}
	return dev.IgnoredFolder(folder)
}

// Device returns the configuration for the given device and an "ok" bool.
func (w *wrapper) Device(id protocol.DeviceID) (DeviceConfiguration, bool) {
	w.mut.Lock()
	defer w.mut.Unlock()
	for _, device := range w.cfg.Devices {
		if device.DeviceID == id {
			return device.Copy(), true
		}
	}
	return DeviceConfiguration{}, false
}

// Folder returns the configuration for the given folder and an "ok" bool.
func (w *wrapper) Folder(id string) (FolderConfiguration, bool) {
	w.mut.Lock()
	defer w.mut.Unlock()
	for _, folder := range w.cfg.Folders {
		if folder.ID == id {
			return folder.Copy(), true
		}
	}
	return FolderConfiguration{}, false
}

// Save writes the configuration to disk, and generates a ConfigSaved event.
func (w *wrapper) Save() error {
	w.mut.Lock()
	defer w.mut.Unlock()

	fd, err := osutil.CreateAtomic(w.path)
	if err != nil {
		l.Debugln("CreateAtomic:", err)
		return err
	}

	if err := w.cfg.WriteXML(fd); err != nil {
		l.Debugln("WriteXML:", err)
		fd.Close()
		return err
	}

	if err := fd.Close(); err != nil {
		l.Debugln("Close:", err)
		return err
	}

	events.Default.Log(events.ConfigSaved, w.cfg)
	return nil
}

func (w *wrapper) GlobalDiscoveryServers() []string {
	var servers []string
	for _, srv := range w.Options().GlobalAnnServers {
		switch srv {
		case "default":
			servers = append(servers, DefaultDiscoveryServers...)
		case "default-v4":
			servers = append(servers, DefaultDiscoveryServersV4...)
		case "default-v6":
			servers = append(servers, DefaultDiscoveryServersV6...)
		default:
			servers = append(servers, srv)
		}
	}
	return util.UniqueTrimmedStrings(servers)
}

func (w *wrapper) ListenAddresses() []string {
	var addresses []string
	for _, addr := range w.Options().ListenAddresses {
		switch addr {
		case "default":
			addresses = append(addresses, DefaultListenAddresses...)
		default:
			addresses = append(addresses, addr)
		}
	}
	return util.UniqueTrimmedStrings(addresses)
}

func (w *wrapper) RequiresRestart() bool {
	return atomic.LoadUint32(&w.requiresRestart) != 0
}

func (w *wrapper) setRequiresRestart() {
	atomic.StoreUint32(&w.requiresRestart, 1)
}

func (w *wrapper) MyName() string {
	w.mut.Lock()
	myID := w.cfg.MyID
	w.mut.Unlock()
	cfg, _ := w.Device(myID)
	return cfg.Name
}

func (w *wrapper) AddOrUpdatePendingDevice(device protocol.DeviceID, name, address string) {
	w.mut.Lock()
	defer w.mut.Unlock()

	for i := range w.cfg.PendingDevices {
		if w.cfg.PendingDevices[i].ID == device {
			w.cfg.PendingDevices[i].Time = time.Now().Round(time.Second)
			w.cfg.PendingDevices[i].Name = name
			w.cfg.PendingDevices[i].Address = address
			return
		}
	}

	w.cfg.PendingDevices = append(w.cfg.PendingDevices, ObservedDevice{
		Time:    time.Now().Round(time.Second),
		ID:      device,
		Name:    name,
		Address: address,
	})
}

func (w *wrapper) AddOrUpdatePendingFolder(id, label string, device protocol.DeviceID) {
	w.mut.Lock()
	defer w.mut.Unlock()

	for i := range w.cfg.Devices {
		if w.cfg.Devices[i].DeviceID == device {
			for j := range w.cfg.Devices[i].PendingFolders {
				if w.cfg.Devices[i].PendingFolders[j].ID == id {
					w.cfg.Devices[i].PendingFolders[j].Label = label
					w.cfg.Devices[i].PendingFolders[j].Time = time.Now().Round(time.Second)
					return
				}
			}
			w.cfg.Devices[i].PendingFolders = append(w.cfg.Devices[i].PendingFolders, ObservedFolder{
				Time:  time.Now().Round(time.Second),
				ID:    id,
				Label: label,
			})
			return
		}
	}

	panic("bug: adding pending folder for non-existing device")
}
