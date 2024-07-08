package main

import (
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/be9/tbc/cmd"
	"github.com/urfave/cli/v2"
)

const (
	VerboseFlag = "verbose"
	SummaryFlag = "summary"

	defaultCacheTimeout = 30 * time.Second
)

func main() {
	var (
		opts              cmd.Options
		certFile, keyFile string

		logger = slog.Default()
	)
	app := &cli.App{
		Name:  "tbc",
		Usage: "TurboRepo <--> Bazel Remote Cache Proxy",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "host",
				EnvVars:     []string{"TBC_HOST"},
				Usage:       "Remote cache server `HOST`",
				Required:    true,
				Aliases:     []string{"H"},
				Destination: &opts.RemoteCacheHost,
			},
			&cli.StringFlag{
				Name:        "addr",
				EnvVars:     []string{"TBC_ADDR"},
				Usage:       "Address to bind to",
				Value:       ":8080",
				Destination: &opts.BindAddr,
			},
			&cli.StringFlag{
				Name:        "tls_client_certificate",
				EnvVars:     []string{"TBC_CLIENT_CERT"},
				Usage:       "TLS certificate `FILE`",
				TakesFile:   true,
				Destination: &certFile,
			},
			&cli.StringFlag{
				Name:        "tls_client_key",
				EnvVars:     []string{"TBC_CLIENT_KEY"},
				Usage:       "TLS key `FILE`",
				TakesFile:   true,
				Destination: &keyFile,
			},
			&cli.DurationFlag{
				Name:        "timeout",
				EnvVars:     []string{"TBC_CLIENT_TIMEOUT"},
				Usage:       "Cache ops timeout",
				Value:       defaultCacheTimeout,
				Destination: &opts.RemoteCacheTimeout,
			},
			&cli.BoolFlag{
				Name:        "auto-env",
				EnvVars:     []string{"TBC_AUTO_ENV"},
				Usage:       "Set up environment for turbo",
				Value:       true,
				Destination: &opts.AutoEnv,
			},

			&cli.BoolFlag{
				Name:    VerboseFlag,
				EnvVars: []string{"TBC_VERBOSE"},
				Aliases: []string{"v"},
				Usage:   "Be more verbose",
			},
			&cli.BoolFlag{
				Name:    SummaryFlag,
				EnvVars: []string{"TBC_SUMMARY"},
				Aliases: []string{"s"},
				Usage:   "Print server summary when the wrapped command exits",
			},
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
			exitCode, stats, err := cmd.Main(logger, opts)
			if err != nil {
				return cli.Exit(err, exitCode)
			}
			if c.Bool(SummaryFlag) {
				logger.Info("server stats", stats.SlogArgs()...)
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
