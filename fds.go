package tableroll

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

var (
	// ErrUpgradeInProgress indicates that an upgrade is in progress. This state
	// is not necessarily terminal.
	// This error will be returned if an attempt is made to mutate the file
	// descriptor store while the upgrader is currently attempting to transfer
	// all file descriptors elsewhere.
	ErrUpgradeInProgress = errors.New("an upgrade is currently in progress")
	// ErrUpgradeCompleted indicates that an upgrade has already happened. This
	// state is terminal.
	// This error will be returned if an attempt is made to mutate the file
	// descriptor store after an upgrade has already completed.
	ErrUpgradeCompleted = errors.New("an upgrade has completed")
	// ErrUpgraderStopped indicates the upgrader's Stop method has been called.
	// This state is terminal.
	// This error will be returned if an atttempt is made to mutate the file
	// descriptor store after stopping the upgrader.
	ErrUpgraderStopped = errors.New("the upgrader has been marked as stopped")
)

// Listener can be shared between processes.
type Listener interface {
	net.Listener
	syscall.Conn
}

// Conn can be shared between processes.
type Conn interface {
	net.Conn
	syscall.Conn
}

type fdKind string

const (
	fdKindListener fdKind = "listener"
	fdKindConn            = "conn"
	fdKindFile            = "file"
)

// file works around the fact that it's not possible
// to get the fd from an os.File without putting it into
// blocking mode.
type file struct {
	*os.File
	fd uintptr
}

func (f *file) String() string {
	name := "<nil>"
	if f != nil && f.File != nil {
		name = f.Name()
	}
	return fmt.Sprintf("File(name=%q,fd=%v)", name, f.fd)
}

func newFile(fd uintptr, name string) *file {
	f := os.NewFile(fd, name)
	if f == nil {
		return nil
	}

	return &file{
		f,
		fd,
	}
}

// fd is the wrapping type for a file.
// It contains metadata about the file to make it easier to log information
// about it, and most importantly keeps a reference to the underlying file
// object.
type fd struct {
	// The underlying file object
	file *file

	Kind fdKind `json:"kind"`
	// ID is the id of this file, stored just for pretty-printing
	ID string `json:"id"`

	// for conns/listeners, stored just for pretty-printing
	Network string `json:"network,omitempty"`
	Addr    string `json:"addr,omitempty"`
}

func (f *fd) String() string {
	switch f.Kind {
	case fdKindFile:
		return fmt.Sprintf("file(%v)", f.ID)
	case fdKindListener:
		return fmt.Sprintf("listener(%v): %v:%v", f.ID, f.Network, f.Addr)
	case fdKindConn:
		return fmt.Sprintf("conn(%v): %v:%v", f.ID, f.Network, f.Addr)
	default:
		return fmt.Sprintf("unknown: %#v", f)
	}
}

// Fds holds all shareable file descriptors, whether created in this process or
// inherited from a previous one.  It provides methods for adding and removing
// file descriptors from the store.
type Fds struct {
	mu sync.Mutex
	// NB: Files in these maps may be in blocking mode.
	fds map[string]*fd

	// locked indicates whether the addition and removal of new listeners is locked.
	// When true, all mutations will result in an error with the error 'lockedReason'
	locked       bool
	lockedReason error

	l log15.Logger
}

func (f *Fds) String() string {
	res := make([]string, 0, len(f.fds))
	for _, fi := range f.fds {
		res = append(res, fi.String())
	}
	return fmt.Sprintf("fds: %v", res)
}

func newFds(l log15.Logger, inherited map[string]*fd) *Fds {
	if inherited == nil {
		inherited = make(map[string]*fd)
	}
	return &Fds{
		fds: inherited,
		l:   l,
	}
}

func (f *Fds) lockMutations(reason error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locked = true
	f.lockedReason = reason
}

func (f *Fds) unlockMutations() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locked = false
	f.lockedReason = nil
}

// Listen returns a listener inherited from the parent process, or creates a
// new one. It is expected that the caller will close the returned listener
// once the Upgrader indicates draining is desired.
// The arguments are passed to net.Listen, and their meaning is described
// there.
func (f *Fds) Listen(ctx context.Context, id string, cfg *net.ListenConfig, network, addr string) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg == nil {
		cfg = &net.ListenConfig{}
	}

	ln, err := f.listenerLocked(id)
	if err != nil {
		return nil, err
	}
	if ln != nil {
		f.l.Debug("found existing listener in store", "listenerId", id, "network", network, "addr", addr)
		return ln, nil
	}

	if f.locked {
		return nil, f.lockedReason
	}

	ln, err = cfg.Listen(ctx, network, addr)
	if err != nil {
		return nil, errors.Wrap(err, "can't create new listener")
	}

	fdLn, ok := ln.(Listener)
	if !ok {
		ln.Close()
		return nil, errors.Errorf("%T doesn't implement tableroll.Listener", ln)
	}

	err = f.addListenerLocked(id, network, addr, fdLn)
	if err != nil {
		fdLn.Close()
		return nil, err
	}

	return ln, nil
}

// ListenWith returns a listener with the given id inherited from the previous
// owner, or if it doesn't exist creates a new one using the provided function.
// The listener function should return quickly since it will block any upgrade
// requests from being serviced.
// Note that any unix sockets will have "SetUnlinkOnClose(false)" called on
// them. Callers may choose to switch them back to 'true' if appropriate.
// The listener function is compatible with net.Listen.
func (f *Fds) ListenWith(id, network, addr string, listenerFunc func(network, addr string) (net.Listener, error)) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ln, err := f.listenerLocked(id)
	if err != nil {
		return nil, err
	}
	if ln != nil {
		return ln, nil
	}
	if f.locked {
		return nil, f.lockedReason
	}

	ln, err = listenerFunc(network, addr)
	if err != nil {
		return nil, err
	}
	if _, ok := ln.(Listener); !ok {
		ln.Close()
		return nil, errors.Errorf("%T doesn't implement tableroll.Listener", ln)
	}
	if err := f.addListenerLocked(id, network, addr, ln.(Listener)); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Listener returns an inherited listener with the given ID, or nil.
//
// It is the caller's responsibility to close the returned listener once
// connections should be drained.
func (f *Fds) Listener(id string) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.listenerLocked(id)
}

func (f *Fds) listenerLocked(id string) (net.Listener, error) {
	file, ok := f.fds[id]
	if !ok || file.file == nil {
		return nil, nil
	}

	ln, err := net.FileListener(file.file.File)
	if err != nil {
		return nil, errors.Wrapf(err, "can't inherit listener %s", file.file)
	}
	return ln, nil
}

type unlinkOnCloser interface {
	SetUnlinkOnClose(bool)
}

func (f *Fds) addListenerLocked(id, addr, network string, ln Listener) error {
	if ifc, ok := ln.(unlinkOnCloser); ok {
		ifc.SetUnlinkOnClose(false)
	}

	return f.addConnLocked(id, fdKindListener, addr, network, ln)
}

// DialWith takess an id and a function that returns a connection (akin to
// net.Dial). If an inherited connection with that id exists, it will be
// returned. Otherwise, the provided function will be called and the resulting
// connection stored with that id and returned.
func (f *Fds) DialWith(id, network, address string, dialFn func(network, address string) (net.Conn, error)) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	conn, err := f.connLocked(id)
	if err != nil {
		return conn, err
	}
	if conn != nil {
		return conn, nil
	}
	if f.locked {
		return nil, f.lockedReason
	}

	newConn, err := dialFn(network, address)
	if err != nil {
		return nil, err
	}
	fdConn, ok := newConn.(Conn)
	if !ok {
		newConn.Close()
		return nil, errors.Errorf("%T doesn't implement tableroll.Conn", newConn)
	}

	if err := f.addConnLocked(id, fdKindConn, network, address, fdConn); err != nil {
		newConn.Close()
		return nil, err
	}
	return newConn, nil
}

// Conn returns an inherited connection or nil.
//
// It is the caller's responsibility to close the returned Conn at the
// appropriate time, typically when the Upgrader indicates draining and exiting
// is expected.
func (f *Fds) Conn(id string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connLocked(id)
}

func (f *Fds) connLocked(id string) (net.Conn, error) {
	file, ok := f.fds[id]
	if !ok || file.file == nil {
		return nil, nil
	}

	conn, err := net.FileConn(file.file.File)
	if err != nil {
		return nil, errors.Wrapf(err, "can't inherit connection %s", file.file)
	}
	return conn, nil
}

func (f *Fds) addConnLocked(id string, kind fdKind, network, addr string, conn syscall.Conn) error {
	fdObj := &fd{
		Kind:    kind,
		ID:      id,
		Network: network,
		Addr:    addr,
	}
	file, err := dupConn(conn, fdObj.String())
	if err != nil {
		return errors.Wrapf(err, "can't dup listener %s %s", network, addr)
	}
	fdObj.file = file
	f.fds[id] = fdObj
	return nil
}

// OpenFileWith retrieves the given file from the store, and if it's not present opens and adds it.
// The required openFunc is compatible with `os.Open`.
func (f *Fds) OpenFileWith(id string, name string, openFunc func(name string) (*os.File, error)) (*os.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fi, err := f.fileLocked(id)
	if err != nil {
		return nil, err
	}
	if fi != nil {
		return fi, nil
	}
	if f.locked {
		return nil, f.lockedReason
	}

	newFi, err := openFunc(name)
	if err != nil {
		return newFi, err
	}

	dup, err := dupFile(newFi, id)
	if err != nil {
		newFi.Close()
		return nil, err
	}

	newFd := &fd{
		ID:   id,
		Kind: fdKindFile,
		file: dup,
	}
	f.fds[id] = newFd

	return newFi, nil
}

// File returns an inherited file or nil.
//
// The descriptor may be in blocking mode.
func (f *Fds) File(id string) (*os.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fileLocked(id)
}

// Remove removes the given file descriptor from the fds store.
func (f *Fds) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// It's unsafe to close a file descriptor during an upgrade, but it's safe to
	// close it after an upgrade is complete, and it's necessary to do so to
	// avoid leaking fds.
	if f.locked && f.lockedReason == ErrUpgradeInProgress {
		return f.lockedReason
	}

	item, ok := f.fds[id]
	if !ok {
		return errors.Errorf("no element in map with id %v", id)
	}
	delete(f.fds, id)
	if item.file != nil {
		return item.file.Close()
	}
	return nil
}

func (f *Fds) fileLocked(id string) (*os.File, error) {
	file, ok := f.fds[id]
	if !ok || file.file == nil {
		return nil, nil
	}

	// Make a copy of the file, since we don't want to
	// allow the caller to invalidate fds in f.inherited.
	dup, err := dupFd(file.file.fd, file.String())
	if err != nil {
		return nil, err
	}
	return dup.File, nil
}

func (f *Fds) copy() map[string]*fd {
	f.mu.Lock()
	defer f.mu.Unlock()

	files := make(map[string]*fd, len(f.fds))
	for key, file := range f.fds {
		files[key] = file
	}

	return files
}

func unlinkUnixSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSocket == 0 {
		return nil
	}

	return os.Remove(path)
}

func dupConn(conn syscall.Conn, name string) (*file, error) {
	// Use SyscallConn instead of File to avoid making the original
	// fd non-blocking.
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var dup *file
	var duperr error
	err = raw.Control(func(fd uintptr) {
		dup, duperr = dupFd(fd, name)
	})
	if err != nil {
		return nil, errors.Wrap(err, "can't access fd")
	}
	return dup, duperr
}

func dupFd(fd uintptr, name string) (*file, error) {
	dupfd, _, errno := unix.Syscall(unix.SYS_FCNTL, fd, unix.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, errors.Wrap(errno, "can't dup fd using fcntl")
	}

	return newFile(dupfd, name), nil
}
