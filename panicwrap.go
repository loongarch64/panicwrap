// The panicwrap package provides functions for capturing and handling
// panics in your application. It does this by re-executing the running
// application and monitoring stderr for any panics. At the same time,
// stdout/stderr/etc. are set to the same values so that data is shuttled
// through properly, making the existence of panicwrap mostly transparent.
//
// Panics are only detected when the subprocess exits with a non-zero
// exit status, since this is the only time panics are real. Otherwise,
// "panic-like" output is ignored.
package panicwrap

import (
	"bytes"
	"errors"
	"github.com/mitchellh/osext"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const (
	DEFAULT_COOKIE_KEY = "cccf35992f8f3cd8d1d28f0109dd953e26664531"
	DEFAULT_COOKIE_VAL = "7c28215aca87789f95b406b8dd91aa5198406750"
)

// HandlerFunc is the type called when a panic is detected.
type HandlerFunc func(string)

// WrapConfig is the configuration for panicwrap when wrapping an existing
// binary. To get started, in general, you only need the BasicWrap function
// that will set this up for you. However, for more customizability,
// WrapConfig and Wrap can be used.
type WrapConfig struct {
	// Handler is the function called when a panic occurs.
	Handler HandlerFunc

	// The cookie key and value are used within environmental variables
	// to tell the child process that it is already executing so that
	// wrap doesn't re-wrap itself.
	CookieKey   string
	CookieValue string
}

// BasicWrap calls Wrap with the given handler function, using defaults
// for everything else. See Wrap and WrapConfig for more information on
// functionality and return values.
func BasicWrap(f HandlerFunc) (int, error) {
	return Wrap(&WrapConfig{
		Handler: f,
	})
}

// Wrap wraps the current executable in a handler to catch panics. It
// returns an error if there was an error during the wrapping process.
// If the error is nil, then the int result indicates the exit status of the
// child process. If the exit status is -1, then this is the child process,
// and execution should continue as normal. Otherwise, this is the parent
// process and the child successfully ran already, and you should exit the
// process with the returned exit status.
//
// This function should be called very very early in your program's execution.
// Ideally, this runs as the first line of code of main.
//
// Once this is called, the given WrapConfig shouldn't be modified or used
// any further.
func Wrap(c *WrapConfig) (int, error) {
	if c.Handler == nil {
		return -1, errors.New("Handler must be set")
	}

	if c.CookieKey == "" {
		c.CookieKey = DEFAULT_COOKIE_KEY
	}

	if c.CookieValue == "" {
		c.CookieValue = DEFAULT_COOKIE_VAL
	}

	// If the cookie key/value match our environment, then we are the
	// child, so just exit now and tell the caller that we're the child
	if os.Getenv(c.CookieKey) == c.CookieValue {
		return -1, nil
	}

	// Get the path to our current executable
	exePath, err := osext.Executable()
	if err != nil {
		return -1, err
	}

	// Pipe the stderr so we can read all the data as we look for panics
	stderr_r, stderr_w := io.Pipe()
	stderrDone := make(chan struct{})
	panicText := new(bytes.Buffer)

	// On close, make sure to finish off the copying of data to stderr
	defer func() {
		stderr_w.Close()
		<-stderrDone

		if panicText.Len() > 0 {
			// We appear to receive a panic... but the program exited normally.
			// Just send the data down to stderr.
			io.Copy(os.Stderr, panicText)
			panicText.Reset()
		}
	}()

	// Start the goroutine that will watch stderr for any panics
	go func() {
		defer close(stderrDone)

		buf := make([]byte, 1024)
		for {
			n, err := stderr_r.Read(buf)
			if n > 0 {
				if panicText.Len() == 0 {
					// We're not currently tracking a panic, determine if we
					// have a panic by looking for "panic:"
					idx := bytes.Index(buf[0:n], []byte("panic:"))
					if idx >= 0 {
						panicText.Write(buf[idx:n])
						n = idx
					}

					os.Stderr.Write(buf[0:n])
				} else {
					panicText.Write(buf[0:n])
				}
			}

			if err == io.EOF {
				break
			}
		}
	}()

	// Build a subcommand to re-execute ourselves. We make sure to
	// set the environmental variable to include our cookie. We also
	// set stdin/stdout to match the config. Finally, we pipe stderr
	// through ourselves in order to watch for panics.
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), c.CookieKey+"="+c.CookieValue)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = stderr_w
	if err := cmd.Start(); err != nil {
		return 1, err
	}

	// Listen to signals and capture them forever. We allow the child
	// process to handle them in some way.
	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		for {
			<-sigCh
		}
	}()

	if err := cmd.Wait(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			// This is some other kind of subprocessing error.
			return 1, err
		}

		exitStatus := 1
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			exitStatus = status.ExitStatus()
		}

		// If we got a panic, then handle it
		if panicText.Len() > 0 {
			c.Handler(panicText.String())
			panicText.Reset()
		}

		return exitStatus, nil
	}

	return 0, nil
}