package sup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/goware/prefixer"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

const VERSION = "0.6.1"

type Stackup struct {
	conf              *Supfile
	debug             bool
	prefix            bool
	ignoreError       bool
	summaryFile       string
	sshConnectTimeout int
}

func New(conf *Supfile) (*Stackup, error) {
	return &Stackup{
		conf: conf,
	}, nil
}

type ClientError struct {
	Host     string `json:"host"`
	Type     string `json:"type"`
	Err      error  `json:"error"`
	ExitCode int    `json:"exit_code"`
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("%s: %v", e.Type, e.Err)
}

func (e *ClientError) normalForm() interface{} {
	if e == nil {
		return nil
	}
	return &struct {
		Host     string `json:"host"`
		Type     string `json:"type"`
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}{
		Host:     e.Host,
		Type:     e.Type,
		Error:    e.Error(),
		ExitCode: e.ExitCode,
	}
}

func (e *ClientError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.normalForm())
}

// Run runs set of commands on multiple hosts defined by network sequentially.
// TODO: This megamoth method needs a big refactor and should be split
//       to multiple smaller methods.
func (sup *Stackup) Run(network *Network, envVars EnvList, commands ...*Command) error {
	if len(commands) == 0 {
		return errors.New("no commands to be run")
	}

	var clientErrors []ClientError

	env := envVars.AsExport()

	// Create clients for every host (either SSH or Localhost).
	var bastion *SSHClient
	if network.Bastion != "" {
		bastion = &SSHClient{
			ConnectTimeout: sup.sshConnectTimeout,
		}
		if err := bastion.Connect(network.Bastion); err != nil {
			return errors.Wrap(err, "connecting to bastion failed")
		}
	}

	var wg sync.WaitGroup
	clientCh := make(chan Client, len(network.Hosts))
	errCh := make(chan ClientError, len(network.Hosts))

	for i, host := range network.Hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()

			// Localhost client.
			if host == "localhost" {
				local := &LocalhostClient{
					env: env + `export SUP_HOST="` + host + `";`,
				}
				if err := local.Connect(host); err != nil {
					errCh <- ClientError{Host: host, Type: "conn", Err: errors.Wrap(err, "connecting to localhost failed"), ExitCode: -1}
					return
				}
				clientCh <- local
				return
			}

			// SSH client.
			remote := &SSHClient{
				env:            env + `export SUP_HOST="` + host + `";`,
				user:           network.User,
				color:          Colors[i%len(Colors)],
				ConnectTimeout: sup.sshConnectTimeout,
			}

			if bastion != nil {
				if err := remote.ConnectWith(host, bastion.DialThrough); err != nil {
					errCh <- ClientError{Host: host, Type: "conn", Err: errors.Wrap(err, "connecting to remote host through bastion failed"), ExitCode: -1}
					return
				}
			} else {
				if err := remote.Connect(host); err != nil {
					errCh <- ClientError{Host: host, Type: "conn", Err: errors.Wrap(err, "connecting to remote host failed"), ExitCode: -1}
					return
				}
			}
			clientCh <- remote
		}(i, host)
	}
	wg.Wait()
	close(clientCh)
	close(errCh)

	maxLen := 0
	var clients []Client
	for client := range clientCh {
		if remote, ok := client.(*SSHClient); ok {
			//lint:ignore SA9001 we want to close the connections at the end of function, not loop, anyways.
			defer remote.Close()
		}
		_, prefixLen := client.Prefix()
		if prefixLen > maxLen {
			maxLen = prefixLen
		}
		clients = append(clients, client)
	}
	for err := range errCh {
		if sup.ignoreError {
			fmt.Fprintf(os.Stderr, "%v\n", err.Err)
			clientErrors = append(clientErrors, err)
		} else {
			return err.Err
		}
	}

	// Run command or run multiple commands defined by target sequentially.
	for _, cmd := range commands {
		// Translate command into task(s).
		tasks, err := sup.createTasks(cmd, clients, env)
		if err != nil {
			return errors.Wrap(err, "creating task failed")
		}

		// Run tasks sequentially.
		for _, task := range tasks {
			var writers []io.Writer
			var wg sync.WaitGroup

			// Run tasks on the provided clients.
			for _, c := range task.Clients {
				var prefix string
				var prefixLen int
				if sup.prefix {
					prefix, prefixLen = c.Prefix()
					if prefixLen < maxLen { // Left padding.
						prefix = strings.Repeat(" ", maxLen-prefixLen) + prefix
					}
				}

				err := c.Run(task)
				if err != nil {
					return errors.Wrap(err, prefix+"task failed")
				}

				// Copy over tasks's STDOUT.
				wg.Add(1)
				go func(c Client) {
					defer wg.Done()
					_, err := io.Copy(os.Stdout, prefixer.New(c.Stdout(), prefix))
					if err != nil && err != io.EOF {
						// TODO: io.Copy() should not return io.EOF at all.
						// Upstream bug? Or prefixer.WriteTo() bug?
						fmt.Fprintf(os.Stderr, "%v", errors.Wrap(err, prefix+"reading STDOUT failed"))
					}
				}(c)

				// Copy over tasks's STDERR.
				wg.Add(1)
				go func(c Client) {
					defer wg.Done()
					_, err := io.Copy(os.Stderr, prefixer.New(c.Stderr(), prefix))
					if err != nil && err != io.EOF {
						fmt.Fprintf(os.Stderr, "%v", errors.Wrap(err, prefix+"reading STDERR failed"))
					}
				}(c)

				writers = append(writers, c.Stdin())
			}

			// Copy over task's STDIN.
			if task.Input != nil {
				go func() {
					var writer io.Writer
					if sup.ignoreError {
						writer = SilentMultiWriter(writers...)
					} else {
						writer = io.MultiWriter(writers...)
					}
					_, err := io.Copy(writer, task.Input)
					if err != nil && err != io.EOF {
						fmt.Fprintf(os.Stderr, "%v", errors.Wrap(err, "copying STDIN failed"))
					}
					// TODO: Use MultiWriteCloser (not in Stdlib), so we can writer.Close() instead?
					for _, c := range clients {
						c.WriteClose()
					}
				}()
			}

			// Catch OS signals and pass them to all active clients.
			trap := make(chan os.Signal, 1)
			signal.Notify(trap, os.Interrupt)
			go func() {
				for {
					sig, ok := <-trap
					if !ok {
						return
					}
					for _, c := range task.Clients {
						err := c.Signal(sig)
						if err != nil {
							fmt.Fprintf(os.Stderr, "%v", errors.Wrap(err, "sending signal failed"))
						}
					}
				}
			}()

			// Wait for all I/O operations first.
			wg.Wait()

			// Make sure each client finishes the task, return on failure.
			for _, c := range task.Clients {
				wg.Add(1)
				go func(c Client) {
					defer wg.Done()
					if err := c.Wait(); err != nil {
						var prefix string
						if sup.prefix {
							var prefixLen int
							prefix, prefixLen = c.Prefix()
							if prefixLen < maxLen { // Left padding.
								prefix = strings.Repeat(" ", maxLen-prefixLen) + prefix
							}
						}
						if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() != 15 {
							// TODO: Store all the errors, and print them after Wait().
							fmt.Fprintf(os.Stderr, "%s%v\n", prefix, e)
							if sup.ignoreError {
								clientErrors = append(clientErrors, ClientError{Host: c.Host(), Type: "run", Err: e, ExitCode: e.ExitStatus()})
							} else {
								os.Exit(e.ExitStatus())
							}
						}
						fmt.Fprintf(os.Stderr, "%s%v\n", prefix, err)

						// TODO: Shouldn't os.Exit(1) here. Instead, collect the exit statuses for later.
						if !sup.ignoreError {
							os.Exit(1)
						}
					}
				}(c)
			}

			// Wait for all commands to finish.
			wg.Wait()

			// Stop catching signals for the currently active clients.
			signal.Stop(trap)
			close(trap)
		}
	}

	if len(clientErrors) > 0 {
		if sup.summaryFile != "" {
			outFile, err := os.OpenFile(sup.summaryFile, os.O_APPEND|os.O_CREATE, 0664)
			if err != nil || outFile == nil {
				fmt.Fprintf(os.Stderr, "could not open summary file '%s' for writing: %v", sup.summaryFile, err.Error())
				os.Exit(2)
			}
			defer outFile.Close()

			data, err := json.MarshalIndent(clientErrors, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "BUG! could not marshal errors json: %s", err.Error())
				os.Exit(2)
			}
			fmt.Fprint(outFile, string(data))
		}

		// TODO: Fix return logic with error types. Is it OK to ignore connection errors?
		for _, e := range clientErrors {
			if e.Type != "conn" {
				os.Exit(1)
			}
		}
	}

	return nil
}

func (sup *Stackup) Debug(value bool) {
	sup.debug = value
}

func (sup *Stackup) Prefix(value bool) {
	sup.prefix = value
}

func (sup *Stackup) IgnoreError(value bool) {
	sup.ignoreError = value
}

func (sup *Stackup) Summary(value string) {
	sup.summaryFile = value
}

func (sup *Stackup) SetSshConnectionTimeout(value int) {
	sup.sshConnectTimeout = value
}
