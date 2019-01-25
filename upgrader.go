package shakiin

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

var (
	stdEnvMu       sync.Mutex
	stdEnvUpgrader *Upgrader
)

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
	parentErr  error              // protected by upgradeSem

	unixSocket string

	upgradeSock *net.UnixListener

	l log15.Logger

	Fds *Fds
}

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

func WithLogger(l log15.Logger) Option {
	return func(u *Upgrader) {
		u.l = l
	}
}

func New(coordinationDir string, opts ...Option) (upg *Upgrader, err error) {
	stdEnvMu.Lock()
	defer stdEnvMu.Unlock()
	if stdEnvUpgrader != nil {
		return nil, errors.New("tableflip: only a single Upgrader allowed")
	}

	upg, err = newUpgrader(coordinationDir, opts...)
	// Store a reference to upg in a private global variable, to prevent
	// it from being GC'ed and exitFd being closed prematurely.
	stdEnvUpgrader = upg
	return
}

func newUpgrader(coordinationDir string, opts ...Option) (*Upgrader, error) {
	upgradeListener, err := listenSock(coordinationDir)
	if err != nil {
		return nil, errors.Wrapf(err, "error listening on upgrade socket")
	}

	coord, parent, files, err := newParent(coordinationDir)
	if err != nil {
		return nil, err
	}

	noopLogger := log15.New()
	noopLogger.SetHandler(log15.DiscardHandler())
	s := &Upgrader{
		upgradeTimeout: DefaultUpgradeTimeout,
		coord:          coord,
		parent:         parent,
		readyC:         make(chan struct{}),
		stopC:          make(chan struct{}),
		upgradeSem:     make(chan struct{}, 1),
		upgradeSock:    upgradeListener,
		exitC:          make(chan struct{}),
		Fds:            newFds(files),
		l:              noopLogger,
	}

	for _, opt := range opts {
		opt(s)
	}

	go func() {
		for {
			err := s.AwaitUpgrade()
			if err != nil {
				// TODO
				panic(err)
			}
		}
	}()

	return s, nil
}

func listenSock(coordinationDir string) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(coordinationDir, os.Getpid())
	return net.ListenUnix("unix", &net.UnixAddr{
		Name: listenpath,
		Net:  "unix",
	})
}

func (u *Upgrader) AwaitUpgrade() error {
	for {
		netConn, err := u.upgradeSock.Accept()
		if err != nil {
			// TODO:
			continue
		}
		// We got a request, only handle one request at a time via semaphore..
		conn := netConn.(*net.UnixConn)

		// Acquire semaphore, but don't block. This allows informing
		// the user that they are doing too many upgrade requests.
		select {
		default:
			// TODO: err
			return errors.New("upgrade in progress")
		case u.upgradeSem <- struct{}{}:
		}

		defer func() {
			<-u.upgradeSem
		}()

		// Make sure we're still ok to perform an upgrade.
		select {
		case <-u.exitC:
			// TODO: err
			return errors.New("already upgraded")
		default:
		}

		select {
		case <-u.readyC:
		default:
			// TODO: err
			return errors.New("TODO")
		}

		// time to pass our FDs along
		nextParent, err := startSibling(conn, u.Fds.copy())
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
