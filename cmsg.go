package tableroll

// Taken from
// https://github.com/opencontainers/runc/blob/cf6c074115d00c932ef01dedb3e13ba8b8f964c3/libcontainer/utils/cmsg.go,
// and modified under the terms of the apache license, 2.0.

/*
 * Copyright 2016, 2017 SUSE LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// oobSpace is the size of the oob slice required to store a single FD. Note
// that unix.UnixRights appears to make the assumption that fd is always int32,
// so sizeof(fd) = 4.
var oobSpace = unix.CmsgSpace(4)

const maxNameLen = 4096

// recvFile receives a '*file' object from a socket that was send using
// 'sendFile'.
// It's identical to the RecvFd call taken from libcontainer, other than
// returning the '*file' type instead of an '*os.File' type.
// This is important because the RecvFd, in returning a '*os.File', requires us
// to call '.Fd()' to get back the underlying file descriptor, which has a
// side-effect of putting the fd into blocking mode.
// We'd rather keep a reference to the fd in our own '*file' struct so we can
// avoid that.
func recvFile(socket *os.File) (*file, error) {
	name := make([]byte, maxNameLen)
	oob := make([]byte, oobSpace)

	sockfd := socket.Fd()
	n, oobn, _, _, err := unix.Recvmsg(int(sockfd), name, oob, 0)
	if err != nil {
		return nil, err
	}

	if n >= maxNameLen || oobn != oobSpace {
		return nil, fmt.Errorf("recvfd: incorrect number of bytes read (n=%d oobn=%d)", n, oobn)
	}

	// Truncate.
	name = name[:n]
	oob = oob[:oobn]

	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	if len(scms) != 1 {
		return nil, fmt.Errorf("recvfd: number of SCMs is not 1: %d", len(scms))
	}
	scm := scms[0]

	fds, err := unix.ParseUnixRights(&scm)
	if err != nil {
		return nil, err
	}
	if len(fds) != 1 {
		return nil, fmt.Errorf("recvfd: number of fds is not 1: %d", len(fds))
	}
	fd := uintptr(fds[0])
	fi := newFile(fd, string(name))
	if fi == nil {
		return nil, fmt.Errorf("could not construct a file")
	}
	return fi, nil
}

// sendFile sends a *file's file descriptor and name over the given socket.
func sendFile(socket *os.File, fi *file) error {
	name := fi.Name()
	if len(name) >= maxNameLen {
		return fmt.Errorf("sendfd: filename too long: %s", fi.Name())
	}
	oob := unix.UnixRights(int(fi.fd))
	return unix.Sendmsg(int(socket.Fd()), []byte(name), oob, nil, 0)
}
