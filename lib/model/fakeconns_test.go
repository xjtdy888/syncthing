// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"bytes"
	"context"
	"net"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/connections"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
)

type downloadProgressMessage struct {
	folder  string
	updates []protocol.FileDownloadProgressUpdate
}

type fakeConnection struct {
	fakeUnderlyingConn
	id                       protocol.DeviceID
	downloadProgressMessages []downloadProgressMessage
	closed                   bool
	files                    []protocol.FileInfo
	fileData                 map[string][]byte
	folder                   string
	model                    *model
	indexFn                  func(string, []protocol.FileInfo)
	requestFn                func(folder, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error)
	closeFn                  func(error)
	mut                      sync.Mutex
}

func (f *fakeConnection) Close(err error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.closeFn != nil {
		f.closeFn(err)
		return
	}
	f.closed = true
	f.model.Closed(f, err)
}

func (f *fakeConnection) Start() {
}

func (f *fakeConnection) ID() protocol.DeviceID {
	return f.id
}

func (f *fakeConnection) Name() string {
	return ""
}

func (f *fakeConnection) Option(string) string {
	return ""
}

func (f *fakeConnection) Index(folder string, fs []protocol.FileInfo) error {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.indexFn != nil {
		f.indexFn(folder, fs)
	}
	return nil
}

func (f *fakeConnection) IndexUpdate(folder string, fs []protocol.FileInfo) error {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.indexFn != nil {
		f.indexFn(folder, fs)
	}
	return nil
}

func (f *fakeConnection) Request(folder, name string, offset int64, size int, hash []byte, weakHash uint32, fromTemporary bool) ([]byte, error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.requestFn != nil {
		return f.requestFn(folder, name, offset, size, hash, fromTemporary)
	}
	return f.fileData[name], nil
}

func (f *fakeConnection) ClusterConfig(protocol.ClusterConfig) {}

func (f *fakeConnection) Ping() bool {
	f.mut.Lock()
	defer f.mut.Unlock()
	return f.closed
}

func (f *fakeConnection) Closed() bool {
	f.mut.Lock()
	defer f.mut.Unlock()
	return f.closed
}

func (f *fakeConnection) Statistics() protocol.Statistics {
	return protocol.Statistics{}
}

func (f *fakeConnection) DownloadProgress(folder string, updates []protocol.FileDownloadProgressUpdate) {
	f.downloadProgressMessages = append(f.downloadProgressMessages, downloadProgressMessage{
		folder:  folder,
		updates: updates,
	})
}

func (f *fakeConnection) addFileLocked(name string, flags uint32, ftype protocol.FileInfoType, data []byte, version protocol.Vector) {
	blockSize := protocol.BlockSize(int64(len(data)))
	blocks, _ := scanner.Blocks(context.TODO(), bytes.NewReader(data), blockSize, int64(len(data)), nil, true)

	if ftype == protocol.FileInfoTypeFile || ftype == protocol.FileInfoTypeDirectory {
		f.files = append(f.files, protocol.FileInfo{
			Name:         name,
			Type:         ftype,
			Size:         int64(len(data)),
			ModifiedS:    time.Now().Unix(),
			Permissions:  flags,
			Version:      version,
			Sequence:     time.Now().UnixNano(),
			RawBlockSize: int32(blockSize),
			Blocks:       blocks,
		})
	} else {
		// Symlink
		f.files = append(f.files, protocol.FileInfo{
			Name:          name,
			Type:          ftype,
			Version:       version,
			Sequence:      time.Now().UnixNano(),
			SymlinkTarget: string(data),
			NoPermissions: true,
		})
	}

	if f.fileData == nil {
		f.fileData = make(map[string][]byte)
	}
	f.fileData[name] = data
}

func (f *fakeConnection) addFile(name string, flags uint32, ftype protocol.FileInfoType, data []byte) {
	f.mut.Lock()
	defer f.mut.Unlock()

	var version protocol.Vector
	version = version.Update(f.id.Short())
	f.addFileLocked(name, flags, ftype, data, version)
}

func (f *fakeConnection) updateFile(name string, flags uint32, ftype protocol.FileInfoType, data []byte) {
	f.mut.Lock()
	defer f.mut.Unlock()

	for i, fi := range f.files {
		if fi.Name == name {
			f.files = append(f.files[:i], f.files[i+1:]...)
			f.addFileLocked(name, flags, ftype, data, fi.Version.Update(f.id.Short()))
			return
		}
	}
}

func (f *fakeConnection) deleteFile(name string) {
	f.mut.Lock()
	defer f.mut.Unlock()

	for i, fi := range f.files {
		if fi.Name == name {
			fi.Deleted = true
			fi.ModifiedS = time.Now().Unix()
			fi.Version = fi.Version.Update(f.id.Short())
			fi.Sequence = time.Now().UnixNano()
			fi.Blocks = nil

			f.files = append(append(f.files[:i], f.files[i+1:]...), fi)
			return
		}
	}
}

func (f *fakeConnection) sendIndexUpdate() {
	f.model.IndexUpdate(f.id, f.folder, f.files)
}

func addFakeConn(m *model, dev protocol.DeviceID) *fakeConnection {
	fc := &fakeConnection{id: dev, model: m}
	m.AddConnection(fc, protocol.HelloResult{})

	m.ClusterConfig(dev, protocol.ClusterConfig{
		Folders: []protocol.Folder{
			{
				ID: "default",
				Devices: []protocol.Device{
					{ID: myID},
					{ID: device1},
				},
			},
		},
	})

	return fc
}

type fakeProtoConn struct {
	protocol.Connection
	fakeUnderlyingConn
}

func newFakeProtoConn(protoConn protocol.Connection) connections.Connection {
	return &fakeProtoConn{Connection: protoConn}
}

// fakeUnderlyingConn implements the methods of connections.Connection that are
// not implemented by protocol.Connection
type fakeUnderlyingConn struct{}

func (f *fakeUnderlyingConn) RemoteAddr() net.Addr {
	return &fakeAddr{}
}

func (f *fakeUnderlyingConn) Type() string {
	return "fake"
}

func (f *fakeUnderlyingConn) Crypto() string {
	return "fake"
}

func (f *fakeUnderlyingConn) Transport() string {
	return "fake"
}

func (f *fakeUnderlyingConn) Priority() int {
	return 9000
}

func (f *fakeUnderlyingConn) String() string {
	return ""
}

type fakeAddr struct{}

func (fakeAddr) Network() string {
	return "network"
}

func (fakeAddr) String() string {
	return "address"
}
