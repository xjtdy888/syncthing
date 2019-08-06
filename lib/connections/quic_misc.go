// Copyright (C) 2019 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// +build go1.12

package connections

import (
	"net"

	"github.com/lucas-clemente/quic-go"
)

var (
	quicConfig = &quic.Config{
		ConnectionIDLength: 4,
		KeepAlive:          true,
	}
)

type quicTlsConn struct {
	quic.Session
	quic.Stream
	// If we created this connection, we should be the ones closing it.
	createdConn net.PacketConn
}

func (q *quicTlsConn) Close() error {
	sterr := q.Stream.Close()
	seerr := q.Session.Close()
	var pcerr error
	if q.createdConn != nil {
		pcerr = q.createdConn.Close()
	}
	if sterr != nil {
		return sterr
	}
	if seerr != nil {
		return seerr
	}
	return pcerr
}

// Sort available packet connections by ip address, preferring unspecified local address.
func packetConnLess(i interface{}, j interface{}) bool {
	iIsUnspecified := false
	jIsUnspecified := false
	iLocalAddr := i.(net.PacketConn).LocalAddr()
	jLocalAddr := j.(net.PacketConn).LocalAddr()

	if host, _, err := net.SplitHostPort(iLocalAddr.String()); err == nil {
		iIsUnspecified = host == "" || net.ParseIP(host).IsUnspecified()
	}
	if host, _, err := net.SplitHostPort(jLocalAddr.String()); err == nil {
		jIsUnspecified = host == "" || net.ParseIP(host).IsUnspecified()
	}

	if jIsUnspecified == iIsUnspecified {
		return len(iLocalAddr.Network()) < len(jLocalAddr.Network())
	}

	return iIsUnspecified
}
