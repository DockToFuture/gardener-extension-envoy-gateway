// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"fmt"
	"time"

	gardenerhealthz "github.com/gardener/gardener/pkg/healthz"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// Options configures the netpol controller's two-cluster manager.
type Options struct {
	// SeedRESTConfig is the connection to the seed cluster. The manager runs
	// against the seed only to host leader election, health, and metrics; it
	// does not watch any seed resources.
	SeedRESTConfig *rest.Config
	// ShootRESTConfig is the connection to the shoot API server, built from the
	// generic kubeconfig the token-requestor projects into the pod.
	ShootRESTConfig *rest.Config
	// SeedNamespace is the shoot's control-plane namespace on the seed
	// (shoot--<project>--<name>); used only to scope leader-election leases.
	SeedNamespace string
	// Experimental enables watching the experimental-channel route kinds
	// (TCPRoute, TLSRoute, UDPRoute). Off by default so an absent CRD does not
	// fail startup.
	Experimental bool
	// LeaderElection enables leader election on the seed. Off by default: the
	// controller runs single-replica, so there is no peer to elect against.
	LeaderElection bool
	// MetricsBindAddress and HealthProbeBindAddress are the manager's serving
	// addresses ("0" disables).
	MetricsBindAddress     string
	HealthProbeBindAddress string
}

// shootScheme returns the scheme the shoot cluster's cache and client use: the
// built-in Kubernetes types (Service, NetworkPolicy) plus the Gateway API types
// the controller watches and resolves. The experimental v1alpha2 route kinds
// are registered only when enabled.
func shootScheme(experimental bool) (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		gatewayv1.Install,
	} {
		if err := add(s); err != nil {
			return nil, fmt.Errorf("failed to build shoot scheme: %w", err)
		}
	}
	if experimental {
		if err := gatewayv1alpha2.Install(s); err != nil {
			return nil, fmt.Errorf("failed to add experimental gateway-api scheme: %w", err)
		}
	}

	return s, nil
}

// NewManager builds the seed manager and the shoot cluster, wires the netpol
// reconciler against the shoot cluster's cache, and returns the manager ready
// to Start. The manager owns the seed connection (lease/health/metrics); the
// shoot cluster is added as a runnable so its cache starts and stops with the
// manager.
func NewManager(ctx context.Context, logger logr.Logger, opts Options) (manager.Manager, error) {
	scheme, err := shootScheme(opts.Experimental)
	if err != nil {
		return nil, err
	}

	// Minimal seed scheme: the manager only needs core types for its lease.
	seedScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(seedScheme); err != nil {
		return nil, fmt.Errorf("failed to build seed scheme: %w", err)
	}

	mgr, err := manager.New(opts.SeedRESTConfig, manager.Options{
		Scheme:                  seedScheme,
		Logger:                  logger,
		Metrics:                 metricsOptions(opts.MetricsBindAddress),
		HealthProbeBindAddress:  opts.HealthProbeBindAddress,
		LeaderElection:          opts.LeaderElection,
		LeaderElectionID:        "envoy-gateway-netpol-controller",
		LeaderElectionNamespace: opts.SeedNamespace,
		BaseContext:             func() context.Context { return ctx },
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create seed manager: %w", err)
	}

	shootCluster, err := cluster.New(opts.ShootRESTConfig, func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create shoot cluster: %w", err)
	}
	if err := mgr.Add(shootCluster); err != nil {
		return nil, fmt.Errorf("failed to add shoot cluster to manager: %w", err)
	}

	reconciler := &Reconciler{
		ShootClient:  shootCluster.GetClient(),
		Experimental: opts.Experimental,
	}
	if err := reconciler.AddToManager(mgr, shootCluster); err != nil {
		return nil, fmt.Errorf("failed to add netpol reconciler: %w", err)
	}

	// Readiness is gated on the shoot cache having synced: until the Gateway and
	// route informers are populated the controller cannot make correct
	// decisions, so it must not report ready.
	if opts.HealthProbeBindAddress != "0" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return nil, fmt.Errorf("failed to add healthz check: %w", err)
		}
		if err := mgr.AddReadyzCheck("shoot-cache-sync", gardenerhealthz.NewCacheSyncHealthzWithDeadline(
			logger, clock.RealClock{}, shootCluster.GetCache(), 5*time.Second,
		)); err != nil {
			return nil, fmt.Errorf("failed to add shoot-cache-sync readyz check: %w", err)
		}
	}

	return mgr, nil
}

// metricsOptions returns the manager's metrics-server options for the given
// bind address ("0" disables the metrics server).
func metricsOptions(addr string) metricsserver.Options {
	return metricsserver.Options{BindAddress: addr}
}
