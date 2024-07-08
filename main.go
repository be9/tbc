package main

import (
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/be9/tbc/cmd"
	"github.com/urfave/cli/v2"
)

func main() {
	var (
		opts              cmd.Options
		certFile, keyFile string
	)
	app := &cli.App{
		Name:  "tbc",
		Usage: "TurboRepo <--> Bazel Remote Cache Proxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: HostFlag, Usage: "Remote cache server `HOST`", Required: true, Aliases: []string{"H"}, Destination: &opts.RemoteCacheHost},
			&cli.StringFlag{Name: AddrFlag, Usage: "Address to bind to", Value: ":8080", Destination: &opts.BindAddr},
			&cli.StringFlag{Name: TLSClientCertFlag, Usage: "TLS certificate `FILE`", TakesFile: true, Destination: &certFile},
			&cli.StringFlag{Name: TLSClientKeyFlag, Usage: "TLS key `FILE`", TakesFile: true, Destination: &keyFile},
			&cli.BoolFlag{Name: VerboseFlag, Aliases: []string{"v"}, Usage: "Be more verbose"},
			&cli.BoolFlag{Name: SummaryFlag, Aliases: []string{"s"}, Usage: "Print server summary when the wrapped command exits"},
			&cli.DurationFlag{Name: TimeoutFlag, Usage: "Cache ops timeout", Value: defaultCacheTimeout, Destination: &opts.RemoteCacheTimeout},
		},
		Before: func(c *cli.Context) error {
			if c.Bool(VerboseFlag) {
				slog.SetLogLoggerLevel(slog.LevelDebug)
			}
			if (certFile != "") != (keyFile != "") {
				return cli.Exit(errors.New("--tls-cert and --tls-key must be provided together"), 1)
			}
			if certFile != "" {
				certPEMBlock, err := os.ReadFile(certFile)
				if err != nil {
					return cli.Exit(err, 1)
				}
				keyPEMBlock, err := os.ReadFile(keyFile)
				if err != nil {
					return cli.Exit(err, 1)
				}
				opts.RemoteCacheTLS = &cmd.TLSCerts{CertPEM: certPEMBlock, KeyPEM: keyPEMBlock}
			}

			return nil
		},
		Action: func(c *cli.Context) error {
			exitCode, stats, err := cmd.Main(opts)
			if err != nil {
				return cli.Exit(err, exitCode)
			}
			if c.Bool(SummaryFlag) {
				slog.Info("server stats", stats.SlogArgs()...)
			}
			os.Exit(exitCode)
			return nil
		},
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

	if err := app.Run(os.Args); err != nil {
		slog.Error(err.Error())
	}
}

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
