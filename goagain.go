// Zero-downtime restarts in Go.
package goagain

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

type strategy int

const (
	// The Single-exec strategy: parent forks child to exec with an inherited
	// net.Listener; child kills parent and becomes a child of init(8).
	Single strategy = iota

	// The Double-exec strategy: parent forks child to exec (first) with an
	// inherited net.Listener; child signals parent to exec (second); parent
	// kills child.
	Double
)

// Don't make the caller import syscall.
const (
	SIGINT  = syscall.SIGINT
	SIGQUIT = syscall.SIGQUIT
	SIGTERM = syscall.SIGTERM
	SIGUSR2 = syscall.SIGUSR2
)

var (
	// OnSIGHUP is the function called when the server receives a SIGHUP
	// signal. The normal use case for SIGHUP is to reload the
	// configuration.
	OnSIGHUP func(l net.Listener) error

	// OnSIGUSR1 is the function called when the server receives a
	// SIGUSR1 signal. The normal use case for SIGUSR1 is to reopen the
	// log files.
	OnSIGUSR1 func(l net.Listener) error

	// The strategy to use; Single by default.
	Strategy strategy = Single

	Logger *log.Logger
)

func init() {
	Logger = log.New(os.Stderr, "", log.LstdFlags)
}

func logln(v ...interface{}) {
	if Logger != nil {
		Logger.Println(v...)
	}
}

// Re-exec this same image without dropping the net.Listener.
func Exec(l net.Listener) error {
	var pid int
	fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if syscall.Getppid() == pid {
		return fmt.Errorf("goagain.Exec called by a child process")
	}
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	if _, err := setEnvs(l); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_SIGNAL",
		fmt.Sprintf("%d", syscall.SIGQUIT),
	); nil != err {
		return err
	}
	logln("re-executing", argv0)
	return syscall.Exec(argv0, os.Args, os.Environ())
}

// Fork and exec this same image without dropping the net.Listener.
func ForkExec(l net.Listener) error {
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	wd, err := os.Getwd()
	if nil != err {
		return err
	}
	fd, err := setEnvs(l)
	if nil != err {
		return err
	}
	if err := os.Setenv("GOAGAIN_PID", ""); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_PPID",
		fmt.Sprint(syscall.Getpid()),
	); nil != err {
		return err
	}
	var sig syscall.Signal
	if Double == Strategy {
		sig = syscall.SIGUSR2
	} else {
		sig = syscall.SIGQUIT
	}
	if err := os.Setenv("GOAGAIN_SIGNAL", fmt.Sprintf("%d", sig)); nil != err {
		return err
	}
	files := make([]*os.File, fd+1)
	files[syscall.Stdin] = os.Stdin
	files[syscall.Stdout] = os.Stdout
	files[syscall.Stderr] = os.Stderr
	addr := l.Addr()
	files[fd] = os.NewFile(
		fd,
		fmt.Sprintf("%s:%s->", addr.Network(), addr.String()),
	)
	p, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   wd,
		Env:   os.Environ(),
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})
	if nil != err {
		return err
	}
	logln("spawned child", p.Pid)
	if err = os.Setenv("GOAGAIN_PID", fmt.Sprint(p.Pid)); nil != err {
		return err
	}
	return nil
}

// Test whether an error is equivalent to net.errClosing as returned by
// Accept during a graceful exit.
func IsErrClosing(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return "use of closed network connection" == err.Error()
}

// Kill process specified in the environment with the signal specified in the
// environment; default to SIGQUIT.
func Kill() error {
	var (
		pid int
		sig syscall.Signal
	)
	_, err := fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if io.EOF == err {
		_, err = fmt.Sscan(os.Getenv("GOAGAIN_PPID"), &pid)
	}
	if nil != err {
		return err
	}
	if _, err := fmt.Sscan(os.Getenv("GOAGAIN_SIGNAL"), &sig); nil != err {
		sig = syscall.SIGQUIT
	}
	if syscall.SIGQUIT == sig && Double == Strategy {
		go syscall.Wait4(pid, nil, 0, nil)
	}
	logln("sending signal", sig, "to process", pid)
	return syscall.Kill(pid, sig)
}

// Reconstruct a net.Listener from a file descriptior and name specified in the
// environment.  Deal with Go's insistence on dup(2)ing file descriptors.
func Listener() (l net.Listener, err error) {
	var fd uintptr
	if _, err = fmt.Sscan(os.Getenv("GOAGAIN_FD"), &fd); nil != err {
		return
	}
	// NewFile takes over the fd but FileListener makes its own copy. Make sure
	// to clean up the former.
	fdf := os.NewFile(fd, os.Getenv("GOAGAIN_NAME"))
	defer fdf.Close()
	l, err = net.FileListener(fdf)
	if nil != err {
		return
	}
	switch l.(type) {
	case *net.TCPListener, *net.UnixListener:
	default:
		err = fmt.Errorf(
			"file descriptor is %T not *net.TCPListener or *net.UnixListener",
			l,
		)
		return
	}
	return
}

// Block this goroutine awaiting signals.  Signals are handled as they
// are by Nginx and Unicorn: <http://unicorn.bogomips.org/SIGNALS.html>.
func Wait(l net.Listener) (syscall.Signal, error) {
	ch := make(chan os.Signal, 2)
	signal.Notify(
		ch,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)
	forked := false
	for {
		sig := <-ch
		logln(sig.String())
		switch sig {

		// SIGHUP should reload configuration.
		case syscall.SIGHUP:
			if nil != OnSIGHUP {
				if err := OnSIGHUP(l); nil != err {
					logln("OnSIGHUP:", err)
				}
			}

		// SIGINT should exit.
		case syscall.SIGINT:
			return syscall.SIGINT, nil

		// SIGQUIT should exit gracefully.
		case syscall.SIGQUIT:
			return syscall.SIGQUIT, nil

		// SIGTERM should exit.
		case syscall.SIGTERM:
			return syscall.SIGTERM, nil

		// SIGUSR1 should reopen logs.
		case syscall.SIGUSR1:
			if nil != OnSIGUSR1 {
				if err := OnSIGUSR1(l); nil != err {
					logln("OnSIGUSR1:", err)
				}
			}

		// SIGUSR2 forks and re-execs the first time it is received and execs
		// without forking from then on.
		case syscall.SIGUSR2:
			if forked {
				return syscall.SIGUSR2, nil
			}
			forked = true
			if err := ForkExec(l); nil != err {
				return syscall.SIGUSR2, err
			}

		}
	}
}

func lookPath() (argv0 string, err error) {
	argv0, err = exec.LookPath(os.Args[0])
	if nil != err {
		return
	}
	if _, err = os.Stat(argv0); nil != err {
		return
	}
	return
}

func setEnvs(l net.Listener) (fd uintptr, err error) {
	var f *os.File
	switch t := l.(type) {
	case *net.TCPListener:
		f, err = t.File()
	case *net.UnixListener:
		f, err = t.File()
	default:
		return fd, fmt.Errorf("setEnvs: file descriptor is %T not *net.TCPListener or *net.UnixListener", l)
	}
	if err != nil {
		return
	}
	fd = f.Fd()
	if err = os.Setenv("GOAGAIN_FD", fmt.Sprint(fd)); nil != err {
		return
	}
	addr := l.Addr()
	if err = os.Setenv(
		"GOAGAIN_NAME",
		fmt.Sprintf("%s:%s->", addr.Network(), addr.String()),
	); nil != err {
		return
	}
	return
}
