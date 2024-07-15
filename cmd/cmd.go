package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Southclaws/fault"
	"github.com/Southclaws/fault/fmsg"
	"github.com/be9/tbc/client"
	"github.com/be9/tbc/server"
	"github.com/hashicorp/go-retryablehttp"
)

// Options carries CLI options, see Main.
type Options struct {
	// The command to run.
	Command string
	// Command's arguments.
	Args []string

	// The remote cache host
	RemoteCacheHost string
	// Timeout used for remote cache operations
	RemoteCacheTimeout time.Duration

	// Certs for TLS (nil means insecure)
	RemoteCacheTLS *TLSCerts

	// The address to bind to
	BindAddr string
	// If true, the command will set TURBO_API, TURBO_TOKEN, and TURBO_TEAM variables (unless they are already set)
	AutoEnv bool
	// If true, just run the command.
	Disabled bool
	// If remote cache connection or proxy server start fails, just run the command.
	IgnoreFailures bool
}

type TLSCerts struct {
	CertPEM, KeyPEM []byte
}

type Cmd struct {
	opts   Options
	logger *slog.Logger
	cl     client.Interface
	srv    *server.Server
}

// Main is the CLI entry.
func Main(
	logger *slog.Logger,
	opts Options,
) (exitCode int, serverStats server.Stats, errorsIgnored bool, err error) {
	var (
		cmd = &Cmd{opts: opts, logger: logger}

		startClientAndServer = func() error {
			var err error
			if err = cmd.instantiateClient(); err != nil {
				return fault.Wrap(err, fmsg.With("failed to create remote cache client"))
			}
			if err = cmd.startServer(); err != nil {
				return fault.Wrap(err, fmsg.With("failed to start proxy server"))
			}
			return nil
		}
	)
	if !cmd.opts.Disabled {
		clientServerErr := startClientAndServer()
		if clientServerErr != nil {
			if cmd.opts.IgnoreFailures {
				logger.Error("cache proxy failed, just running the command",
					slog.String("err", clientServerErr.Error()))

				errorsIgnored = true
			} else {
				return 1, serverStats, false, clientServerErr
			}
		}
	}

	serverActuallyRuns := !errorsIgnored && !cmd.opts.Disabled

	// Start the command in the background
	c := exec.Command(cmd.opts.Command, cmd.opts.Args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	if serverActuallyRuns && cmd.opts.AutoEnv {
		c.Env = cmd.turboEnvironment()
	}
	if err = c.Start(); err != nil {
		return 1, server.Stats{}, false, fault.Wrap(err, fmsg.With("error starting command"))
	}
	if err = c.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			return 1, server.Stats{}, false, fault.Wrap(err, fmsg.With("error running command"))
		}
	}
	if serverActuallyRuns {
		serverStats = cmd.srv.GetStatistics()
	}
	return
}

// instantiateClient creates the client connection and runs CheckCapabilities
func (cmd *Cmd) instantiateClient() error {
	var certPEM, keyPEM []byte

	if cmd.opts.RemoteCacheTLS != nil {
		certPEM = cmd.opts.RemoteCacheTLS.CertPEM
		keyPEM = cmd.opts.RemoteCacheTLS.KeyPEM
	}

	cc, err := client.NewClientConn(cmd.opts.RemoteCacheHost, certPEM, keyPEM)
	if err != nil {
		return err
	}
	cl := client.NewClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), cmd.opts.RemoteCacheTimeout)
	defer cancel()

	cmd.logger.Debug("checking server capabilities")
	if err = cl.CheckCapabilities(ctx); err != nil {
		return err
	}

	cmd.cl = cl
	return nil
}

// startServer creates the server, starts HTTP listener in a goroutine, and uses HTTP GET
// with retries to check that the server is up.
func (cmd *Cmd) startServer() error {
	srv := server.NewServer(cmd.logger, cmd.cl, server.Options{}) // the token is not used

	addr := cmd.opts.BindAddr
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.CreateHandler(),
	}

	go func() {
		cmd.logger.Debug("starting HTTP server", slog.String("addr", addr))

		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// we can't directly signal this error from the goroutine, but in case this happens,
			// the accessibility check will fail.
			cmd.logger.Error(err.Error())
		}
	}()

	hc := retryablehttp.NewClient()
	hc.Logger = nil
	if resp, err := hc.Get(serverCheckURL(addr)); err != nil {
		return err
	} else {
		_ = resp.Body.Close()
		cmd.logger.Debug("HTTP server is accessible", slog.Int("status", resp.StatusCode))
	}

	cmd.srv = srv
	return nil
}

func serverCheckURL(addr string) string {
	return serverBaseURL(addr) + "/v8/artifacts/status"
}

func serverBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	return fmt.Sprintf("http://%s", addr)
}

func (cmd *Cmd) turboEnvironment() []string {
	var (
		env = os.Environ()
		ok  bool
	)
	if _, ok = os.LookupEnv("TURBO_API"); !ok {
		env = append(env, fmt.Sprintf("TURBO_API=%s", serverBaseURL(cmd.opts.BindAddr)))
	}
	if _, ok = os.LookupEnv("TURBO_TOKEN"); !ok {
		env = append(env, "TURBO_TOKEN=ignore")
	}
	if _, ok = os.LookupEnv("TURBO_TEAM"); !ok {
		env = append(env, "TURBO_TEAM=ignore")
	}
	return env
}
