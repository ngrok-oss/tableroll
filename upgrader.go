package tableroll

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
)

// DefaultUpgradeTimeout is the duration before the Upgrader kills the new process if no
// readiness notification was received.
const DefaultUpgradeTimeout time.Duration = time.Minute

// Upgrader handles zero downtime upgrades and passing files between processes.
type Upgrader struct {
	upgradeTimeout time.Duration

	parent     *parent
	coord      *coordinator
	readyOnce  sync.Once
	readyC     chan struct{}
	stopOnce   sync.Once
	stopC      chan struct{}
	upgradeSem chan struct{}
	exitC      chan struct{}      // only close this if holding upgradeSem
	exitFd     neverCloseThisFile // protected by upgradeSem

	upgradeSock *net.UnixListener

	l log15.Logger

	Fds *Fds

	// mocks
	os osIface
}

// Option is an option function for Upgrader.
// See Rob Pike's post on the topic for more information on this pattern:
// https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html
type Option func(u *Upgrader)

// WithUpgradeTimeout allows configuring the update timeout. If a time of 0 is
// specified, the default will be used.
func WithUpgradeTimeout(t time.Duration) Option {
	return func(u *Upgrader) {
		u.upgradeTimeout = t
		if u.upgradeTimeout <= 0 {
			u.upgradeTimeout = DefaultUpgradeTimeout
		}
	}
}

// WithLogger configures the logger to use for tableroll operations.
// By default, nothing will be logged.
func WithLogger(l log15.Logger) Option {
	return func(u *Upgrader) {
		u.l = l
	}
}

// New constructs a tableroll upgrader.
// The first argument is a directory. All processes in an upgrade chain must
// use the same coordination directory. The provided directory must exist and
// be writeable by the process using tableroll.
// Canonically, this directory is `/run/${program}/tableroll/`.
// Any number of options to configure tableroll may also be provided.
func New(coordinationDir string, opts ...Option) (upg *Upgrader, err error) {
	return newUpgrader(realOS{}, coordinationDir, opts...)
}

func newUpgrader(os osIface, coordinationDir string, opts ...Option) (upg *Upgrader, err error) {
	upgradeListener, err := listenSock(os, coordinationDir)
	if err != nil {
		return nil, errors.Wrapf(err, "error listening on upgrade socket")
	}

	noopLogger := log15.New()
	noopLogger.SetHandler(log15.DiscardHandler())
	s := &Upgrader{
		upgradeTimeout: DefaultUpgradeTimeout,
		readyC:         make(chan struct{}),
		stopC:          make(chan struct{}),
		upgradeSem:     make(chan struct{}, 1),
		upgradeSock:    upgradeListener,
		exitC:          make(chan struct{}),
		l:              noopLogger,
		os:             realOS{},
	}
	for _, opt := range opts {
		opt(s)
	}

	coord, parent, files, err := newParent(s.l, s.os, coordinationDir)
	if err != nil {
		return nil, err
	}
	s.coord = coord
	s.parent = parent
	s.Fds = newFds(files)

	go func() {
		for {
			err := s.awaitUpgrade()
			if err != nil {
				s.l.Error("error awaiting upgrade", "err", err)
			}
		}
	}()

	return s, nil
}

func listenSock(os osIface, coordinationDir string) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(coordinationDir, os.Getpid())
	return net.ListenUnix("unix", &net.UnixAddr{
		Name: listenpath,
		Net:  "unix",
	})
}

func (u *Upgrader) awaitUpgrade() error {
	for {
		netConn, err := u.upgradeSock.Accept()
		if err != nil {
			return errors.Wrap(err, "error accepting upgrade socket request")
		}
		conn := netConn.(*net.UnixConn)

		// We got a request, only handle one request at a time via semaphore..
		// Acquire semaphore, but don't block. This allows informing
		// the user that they are doing too many upgrade requests.
		select {
		default:
			return errors.New("upgrade in progress")
		case u.upgradeSem <- struct{}{}:
		}

		defer func() {
			<-u.upgradeSem
		}()

		// Make sure we're still ok to perform an upgrade.
		select {
		case <-u.exitC:
			return errors.New("already upgraded")
		default:
		}

		select {
		case <-u.readyC:
		default:
			// TODO: err
			return errors.New("TODO")
		}

		u.l.Info("passing along the torch")
		// time to pass our FDs along
		nextParent, err := startSibling(u.l, conn, u.Fds.copy())
		if err != nil {
			return errors.Wrap(err, "can't start next parent")
		}

		readyTimeout := time.After(u.upgradeTimeout)
		select {
		case err := <-nextParent.exitedC:
			if err == nil {
				return errors.Errorf("next parent %s exited", nextParent)
			}
			return errors.Wrapf(err, "next parent %s exited", nextParent)

		case <-u.stopC:
			return errors.New("terminating")

		case <-readyTimeout:
			return errors.Errorf("new parent %s timed out", nextParent)

		case file := <-nextParent.readyC:
			// Save file in exitFd, so that it's only closed when the process
			// exits. This signals to the new process that the old process
			// has exited.
			// TODO: is this a thing?
			u.exitFd = neverCloseThisFile{file}
			close(u.exitC)
			return nil
		}
	}
}

// Ready signals that the current process is ready to accept connections.
// It must be called to finish the upgrade.
//
// All fds which were inherited but not used are closed after the call to Ready.
func (u *Upgrader) Ready() error {
	u.readyOnce.Do(func() {
		u.Fds.closeInherited()
		close(u.readyC)
	})

	if u.parent == nil {
		// we are the parent!
		if err := u.coord.BecomeParent(); err != nil {
			return err
		}
		if err := u.coord.Unlock(); err != nil {
			return err
		}
		return nil
	}
	return u.parent.sendReady()
}

// Exit returns a channel which is closed when the process should
// exit.
func (u *Upgrader) Exit() <-chan struct{} {
	return u.exitC
}

// Stop prevents any more upgrades from happening, and closes
// the exit channel.
func (u *Upgrader) Stop() {
	u.stopOnce.Do(func() {
		// Interrupt any running Upgrade(), and
		// prevent new upgrade from happening.
		close(u.stopC)

		// Make sure exitC is closed if no upgrade was running.
		u.upgradeSem <- struct{}{}
		select {
		case <-u.exitC:
		default:
			close(u.exitC)
		}
		<-u.upgradeSem

		u.Fds.closeUsed()
	})
}

// This file must never be closed by the Go runtime, since its used by the
// child to determine when the parent has died. It must only be closed
// by the OS.
// Hence we make sure that this file can't be garbage collected by referencing
// it from an Upgrader.
type neverCloseThisFile struct {
	file *os.File
}
