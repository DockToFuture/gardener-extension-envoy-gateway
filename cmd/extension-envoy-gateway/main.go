// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/urfave/cli/v3"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	managercmd "github.com/gardener/gardener-extension-envoy-gateway/cmd/extension-envoy-gateway/internal/manager"
	webhookcmd "github.com/gardener/gardener-extension-envoy-gateway/cmd/extension-envoy-gateway/webhook"
	"github.com/gardener/gardener-extension-envoy-gateway/pkg/version"
)

func main() {
	app := &cli.Command{
		Name:                  "gardener-extension-envoy-gateway",
		Version:               version.Version,
		EnableShellCompletion: true,
		Usage:                 "envoy-gateway extension for Gardener",
		Commands: []*cli.Command{
			managercmd.New(),
			webhookcmd.New(),
		},
	}

	ctx := ctrl.SetupSignalHandler()
	if err := app.Run(ctx, os.Args); err != nil {
		ctrllog.Log.Error(err, "failed to start envoy-gateway extension")
		os.Exit(1)
	}
}
