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

	"github.com/be9/tbc/client"
	"github.com/be9/tbc/server"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/urfave/cli/v2"
)

const (
	TLSClientCertFlag = "tls_client_certificate"
	TLSClientKeyFlag  = "tls_client_key"
	AddrFlag          = "addr"
	HostFlag          = "host"
	VerboseFlag       = "verbose"
	SummaryFlag       = "summary"
	TimeoutFlag       = "timeout"

	defaultCacheTimeout = 30 * time.Second
)

// CreateApp instantiates cli.App.
func CreateApp() *cli.App {
	app := &cli.App{
		Name:  "tbc",
		Usage: "TurboRepo <--> Bazel Remote Cache Proxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: HostFlag, Usage: "Remote cache server `HOST`", Required: true, Aliases: []string{"H"}},
			&cli.StringFlag{Name: AddrFlag, Usage: "Address to bind to", Value: ":8080"},
			&cli.StringFlag{Name: TLSClientCertFlag, Usage: "TLS certificate `FILE`", TakesFile: true},
			&cli.StringFlag{Name: TLSClientKeyFlag, Usage: "TLS key `FILE`", TakesFile: true},
			&cli.BoolFlag{Name: VerboseFlag, Aliases: []string{"v"}, Usage: "Be more verbose"},
			&cli.BoolFlag{Name: SummaryFlag, Aliases: []string{"s"}, Usage: "Print server summary when the wrapped command exits"},
			&cli.DurationFlag{Name: TimeoutFlag, Usage: "Cache ops timeout", Value: defaultCacheTimeout},
		},
		Before: func(c *cli.Context) error {
			if c.Bool(VerboseFlag) {
				slog.SetLogLoggerLevel(slog.LevelDebug)
			}
			return nil
		},
		Action:          runProxy,
		HideHelpCommand: true,
		ArgsUsage:       "command <command arguments>",
		Description: `Spin up a Turborepo-compatible remote cache server that forwards requests to a Bazel-compatible remote cache server 
and execute the provided command.

Examples:

# Check the server with curl (by default, the server binds to 0.0.0.0:8080)
tbc --host bazel-cache-host:port curl http://localhost:8080/v8/artifacts/status

# Run 'turbo build'
env TURBO_REMOTE_CACHE_SIGNATURE_KEY=super_secret \
    TURBO_API=http://localhost:8080 \
    TURBO_TOKEN=any \		# this is not actually used, but required to be set by turbo
    TURBO_TEAM=any \
    tbc --host bazel-cache-host:port \
    --summary \
    pnpm turbo build
`,
	}
	return app
}

// the main command body
func runProxy(c *cli.Context) error {
	args := c.Args()
	if !args.Present() {
		return cli.Exit("command is not provided", 1)
	}

	cl, err := newClient(c)
	if err != nil {
		return cli.Exit(fmt.Errorf("failed to create remote cache client: %w", err), 1)
	}

	srv, err := newServer(c, cl)
	if err != nil {
		return cli.Exit(fmt.Errorf("failed to start proxy server: %w", err), 1)
	}

	// Start the command in the background
	cmd := exec.Command(c.Args().First(), c.Args().Tail()...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		return cli.Exit(fmt.Errorf("error starting command: %w", err), 1)
	}

	exitCode := 0

	if err = cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			return cli.Exit(fmt.Errorf("error running command: %w", err), 1)
		}
	}

	if c.Bool(SummaryFlag) {
		slog.Info("server stats", srv.GetStatistics().SlogArgs()...)
	}

	os.Exit(exitCode)
	return nil
}

// newClient creates the client connection and runs CheckCapabilities
func newClient(c *cli.Context) (client.Interface, error) {
	cc, err := client.DialGrpc(c.String(HostFlag), c.String(TLSClientCertFlag), c.String(TLSClientKeyFlag))
	if err != nil {
		return nil, err
	}
	cl := client.NewClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), c.Duration(TimeoutFlag))
	defer cancel()

	slog.Debug("checking server capabilities")
	if err = cl.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	return cl, nil
}

// newServer creates the server, starts HTTP listener in a goroutine, and uses HTTP GET
// with retries to check that the server is up.
func newServer(c *cli.Context, cl client.Interface) (*server.Server, error) {
	srv := server.NewServer(cl, server.Options{}) // the token is not used

	addr := c.String(AddrFlag)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.CreateHandler(),
	}

	go func() {
		slog.Debug("starting HTTP server", slog.String("addr", addr))

		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// if listen has failed, we need to terminate the process
			slog.Error(err.Error())
			os.Exit(1)
		}
	}()

	hc := retryablehttp.NewClient()
	hc.Logger = nil
	if resp, err := hc.Get(serverCheckURL(addr)); err != nil {
		return nil, err
	} else {
		_ = resp.Body.Close()
		slog.Debug("HTTP server is accessible", slog.Int("status", resp.StatusCode))
	}
	return srv, nil
}

func serverCheckURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	return fmt.Sprintf("http://%s/v8/artifacts/status", addr)
}
