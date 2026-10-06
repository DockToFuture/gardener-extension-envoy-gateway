// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package netpolcontroller provides the CLI command that runs the per-shoot
// data-plane NetworkPolicy controller in the shoot's control-plane namespace on
// the seed.
package netpolcontroller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	glogger "github.com/gardener/gardener/pkg/logger"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/netpolcontroller"
)

// flags stores the netpol-controller flags as provided from the command-line.
type flags struct {
	seedNamespace       string
	shootKubeconfig     string
	experimental        bool
	leaderElection      bool
	metricsBindAddr     string
	healthProbeBindAddr string
	zapLogLevel         string
	zapLogFormat        string
}

// New creates a new [cli.Command] for running the data-plane NetworkPolicy
// controller.
func New() *cli.Command {
	f := flags{}

	return &cli.Command{
		Name:  "netpol-controller",
		Usage: "start the per-shoot data-plane NetworkPolicy controller",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "seed-namespace",
				Usage:       "the shoot's control-plane namespace on the seed (shoot--<project>--<name>)",
				Sources:     cli.EnvVars("SEED_NAMESPACE"),
				Destination: &f.seedNamespace,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "shoot-kubeconfig",
				Usage:       "path to the generic shoot kubeconfig projected by the token-requestor",
				Value:       gardenerutils.PathGenericKubeconfig,
				Sources:     cli.EnvVars("SHOOT_KUBECONFIG"),
				Destination: &f.shootKubeconfig,
			},
			&cli.BoolFlag{
				Name:        "experimental-channel",
				Usage:       "watch experimental-channel route kinds (TCPRoute, TLSRoute, UDPRoute)",
				Value:       false,
				Sources:     cli.EnvVars("EXPERIMENTAL_CHANNEL"),
				Destination: &f.experimental,
			},
			&cli.BoolFlag{
				Name:        "leader-election",
				Usage:       "enable leader election on the seed (off for single-replica)",
				Value:       false,
				Sources:     cli.EnvVars("LEADER_ELECTION"),
				Destination: &f.leaderElection,
			},
			&cli.StringFlag{
				Name:        "metrics-bind-address",
				Usage:       "the address the metrics endpoint binds to ('0' to disable)",
				Value:       ":8080",
				Sources:     cli.EnvVars("METRICS_BIND_ADDRESS"),
				Destination: &f.metricsBindAddr,
			},
			&cli.StringFlag{
				Name:        "health-probe-bind-address",
				Usage:       "the address the probe endpoint binds to ('0' to disable)",
				Value:       ":8081",
				Sources:     cli.EnvVars("HEALTH_PROBE_BIND_ADDRESS"),
				Destination: &f.healthProbeBindAddr,
			},
			&cli.StringFlag{
				Name:  "log-level",
				Usage: "Zap level to configure the verbosity of logging",
				Value: glogger.InfoLevel,
				Validator: func(val string) error {
					if !slices.Contains(glogger.AllLogLevels, val) {
						return errors.New("invalid log level specified")
					}

					return nil
				},
				Destination: &f.zapLogLevel,
			},
			&cli.StringFlag{
				Name:  "log-format",
				Usage: "Zap log encoding format, json or text",
				Value: glogger.FormatText,
				Validator: func(val string) error {
					if !slices.Contains(glogger.AllLogFormats, val) {
						return errors.New("invalid log format specified")
					}

					return nil
				},
				Destination: &f.zapLogFormat,
			},
		},
		Action: f.run,
	}
}

// run builds the two-cluster manager and starts it.
func (f *flags) run(ctx context.Context, _ *cli.Command) error {
	ctrllog.SetLogger(glogger.MustNewZapLogger(f.zapLogLevel, f.zapLogFormat))
	logger := ctrllog.Log.WithName("netpol-controller")

	seedConfig, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get seed REST config: %w", err)
	}

	shootConfig, err := clientcmd.BuildConfigFromFlags("", f.shootKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build shoot REST config from %q: %w", f.shootKubeconfig, err)
	}

	mgr, err := netpolcontroller.NewManager(ctx, logger, netpolcontroller.Options{
		SeedRESTConfig:         seedConfig,
		ShootRESTConfig:        shootConfig,
		SeedNamespace:          f.seedNamespace,
		Experimental:           f.experimental,
		LeaderElection:         f.leaderElection,
		MetricsBindAddress:     f.metricsBindAddr,
		HealthProbeBindAddress: f.healthProbeBindAddr,
	})
	if err != nil {
		return fmt.Errorf("failed to create netpol-controller manager: %w", err)
	}

	logger.Info("starting netpol-controller", "seedNamespace", f.seedNamespace, "experimental", f.experimental)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("failed to start netpol-controller manager: %w", err)
	}

	return nil
}
