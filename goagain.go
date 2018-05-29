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
	"time"
)

var Logger *log.Logger

func init() {
	Logger = log.New(os.Stderr, "", log.LstdFlags)
}

func logln(v ...interface{}) {
	if Logger != nil {
		Logger.Println(v...)
	}
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
func Kill(sig syscall.Signal) error {
	var (
		pid int
	)
	_, err := fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if io.EOF == err {
		_, err = fmt.Sscan(os.Getenv("GOAGAIN_PPID"), &pid)
	}
	if nil != err {
		return err
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

// Fork and exec this same image without dropping the net.Listener.
func forkExec(l net.Listener, quitSignal syscall.Signal) (*os.Process, error) {
	argv0, err := lookPath()
	if nil != err {
		return nil, err
	}
	wd, err := os.Getwd()
	if nil != err {
		return nil, err
	}
	fd, err := setEnvs(l)
	if nil != err {
		return nil, err
	}
	if err := os.Setenv("GOAGAIN_PID", ""); nil != err {
		return nil, err
	}
	if err := os.Setenv(
		"GOAGAIN_PPID",
		fmt.Sprint(syscall.Getpid()),
	); nil != err {
		return nil, err
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
		return nil, err
	}
	logln("spawned child", p.Pid)
	if err = os.Setenv("GOAGAIN_PID", fmt.Sprint(p.Pid)); nil != err {
		return p, err
	}
	return p, nil
}

func Wait(l net.Listener, forkSignal syscall.Signal, quitSignal syscall.Signal, timeout time.Duration) error {
	forkCh := make(chan os.Signal, 1)
	signal.Notify(forkCh, forkSignal)

	logln("Waiting for fork signal from system...")

	<-forkCh

	cp, err := forkExec(l, quitSignal)
	if err != nil {
		logln(err)

		if cp != nil {
			kErr := cp.Kill()
			if kErr != nil {
				logln("Unable to kill process after bad forkExec", kErr)
			}
		}

		return err
	}

	logln("Waiting for quit signal from child...")

	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, quitSignal)

	select {
	case <-quitCh:
		logln("Received quit signal from child.")
	case <-time.After(timeout):
		logln("Received quit signal from child.")
		err = cp.Kill()
		if err != nil {
			logln("Unable to kill process after timeout", err)
		}
		return fmt.Errorf("Timed out waiting for child to send signal.")
	}

	return nil
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
