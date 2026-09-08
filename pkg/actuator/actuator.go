// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package actuator provides the implementation of a Gardener extension
// actuator for deploying Envoy Gateway and the Gateway API CRDs to shoot
// clusters.
package actuator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionssecretmanager "github.com/gardener/gardener/extensions/pkg/util/secret/manager"
	v1beta1helper "github.com/gardener/gardener/pkg/api/core/v1beta1/helper"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	secretsutils "github.com/gardener/gardener/pkg/utils/secrets"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/component-base/featuregate"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/apis/config"
	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
	"github.com/gardener/gardener-extension-envoy-gateway/pkg/metrics"
)

// ErrInvalidActuator is returned when creating an [Actuator] with invalid
// config settings.
var ErrInvalidActuator = errors.New("invalid actuator")

const (
	// Name is the name of the actuator.
	Name = "envoy-gateway"
	// ExtensionType is the type of the extension resource the actuator reconciles.
	ExtensionType = "envoy-gateway"
	// FinalizerSuffix is the finalizer suffix used by the actuator.
	FinalizerSuffix = "gardener-extension-envoy-gateway"
)

// Actuator is an implementation of [extension.Actuator].
type Actuator struct {
	client      client.Client
	decoder     runtime.Decoder
	imageVector imagevector.ImageVector

	gardenerVersion       string
	gardenletFeatureGates map[featuregate.Feature]bool

	gatewayLister GatewayLister
}

var _ extension.Actuator = &Actuator{}

// Option configures the [Actuator].
type Option func(a *Actuator) error

// New creates a new actuator with the given options.
func New(c client.Client, imageVector imagevector.ImageVector, opts ...Option) (*Actuator, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: no client specified", ErrInvalidActuator)
	}
	if imageVector == nil {
		return nil, fmt.Errorf("%w: no image vector specified", ErrInvalidActuator)
	}

	act := &Actuator{
		client:                c,
		imageVector:           imageVector,
		gardenletFeatureGates: make(map[featuregate.Feature]bool),
	}

	for _, opt := range opts {
		if err := opt(act); err != nil {
			return nil, err
		}
	}

	if act.decoder == nil {
		act.decoder = serializer.NewCodecFactory(c.Scheme(), serializer.EnableStrict).UniversalDecoder()
	}

	if act.gatewayLister == nil {
		act.gatewayLister = NewRealGatewayLister(c)
	}

	return act, nil
}

// WithDecoder configures the [Actuator] with the given [runtime.Decoder].
func WithDecoder(d runtime.Decoder) Option {
	return func(a *Actuator) error {
		a.decoder = d

		return nil
	}
}

// WithGardenerVersion configures the [Actuator] with the given Gardener version.
func WithGardenerVersion(v string) Option {
	return func(a *Actuator) error {
		a.gardenerVersion = v

		return nil
	}
}

// WithGardenletFeatures configures the [Actuator] with the given gardenlet feature gates.
func WithGardenletFeatures(feats map[featuregate.Feature]bool) Option {
	return func(a *Actuator) error {
		a.gardenletFeatureGates = feats

		return nil
	}
}

// WithGatewayLister configures the [Actuator] with a custom [GatewayLister].
// Useful in tests to inject a fake. Production code does not need this — the
// constructor builds a real lister by default.
func WithGatewayLister(l GatewayLister) Option {
	return func(a *Actuator) error {
		a.gatewayLister = l

		return nil
	}
}

// Name returns the name of the actuator.
func (a *Actuator) Name() string { return Name }

// FinalizerSuffix returns the finalizer suffix to use for the actuator.
func (a *Actuator) FinalizerSuffix() string { return FinalizerSuffix }

// ExtensionType returns the type of extension resources the actuator reconciles.
func (a *Actuator) ExtensionType() string { return ExtensionType }

// ExtensionClass returns the [extensionsv1alpha1.ExtensionClass] for the actuator.
func (a *Actuator) ExtensionClass() extensionsv1alpha1.ExtensionClass {
	return extensionsv1alpha1.ExtensionClassShoot
}

// Reconcile reconciles the Extension resource by deploying Envoy Gateway,
// the Gateway API CRDs, and the envoy-gateway GatewayClass to the shoot cluster.
func (a *Actuator) Reconcile(ctx context.Context, logger logr.Logger, ex *extensionsv1alpha1.Extension) (retErr error) {
	clusterName := ex.Namespace
	start := time.Now()
	defer func() {
		recordOperation(clusterName, "reconcile", start, retErr)
	}()

	logger.Info("reconciling envoy-gateway extension", "name", ex.Name, "cluster", clusterName)

	cluster, err := extensionscontroller.GetCluster(ctx, a.client, clusterName)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	if cluster.Shoot.DeletionTimestamp != nil {
		logger.Info("shoot is being deleted, skipping envoy-gateway reconciliation", "cluster", clusterName)

		return nil
	}

	if v1beta1helper.HibernationIsEnabled(cluster.Shoot) {
		logger.Info("shoot is hibernated, skipping envoy-gateway deployment", "cluster", clusterName)

		return nil
	}

	egConfig := envoygateway.DefaultConfig()
	if ex.Spec.ProviderConfig != nil {
		var cfg config.EnvoyGatewayConfig
		// Fail loudly on decode errors rather than silently falling back to
		// defaults — a user who set providerConfig expects their settings to
		// apply, and swallowing the error produces a "the extension ignored my
		// config" bug that is very hard to spot. The Extension resource
		// surfaces the returned error via its status conditions.
		if err := runtime.DecodeInto(a.decoder, ex.Spec.ProviderConfig.Raw, &cfg); err != nil {
			return fmt.Errorf("failed to decode envoy-gateway providerConfig: %w", err)
		}

		if cfg.Channel != "" {
			if _, ok := envoygateway.ValidChannels[string(cfg.Channel)]; !ok {
				return fmt.Errorf("invalid envoy-gateway channel %q: must be one of standard, experimental", cfg.Channel)
			}
			egConfig.ExperimentalFeatures = cfg.Channel == config.ChannelExperimental
		}

		// manageCRDs defaults to true; only an explicit false disables CRD delivery.
		if cfg.ManageCRDs != nil {
			egConfig.ManageCRDs = *cfg.ManageCRDs
		}

		if cfg.ControlPlane != nil {
			if err := applyLogLevel(cfg.ControlPlane.LogLevel, &egConfig.ControlPlaneLogLevel); err != nil {
				return err
			}
			if cfg.ControlPlane.Replicas != nil {
				egConfig.ControlPlaneReplicas = cfg.ControlPlane.Replicas
			}
		}
		if cfg.DataPlane != nil {
			if err := applyLogLevel(cfg.DataPlane.LogLevel, &egConfig.DataPlaneLogLevel); err != nil {
				return err
			}
			if cfg.DataPlane.Replicas != nil {
				egConfig.DataPlaneReplicas = cfg.DataPlane.Replicas
			}
		}

		egConfig.EnvoyProxyDefaults = cfg.EnvoyProxyDefaults
	}

	deployer := envoygateway.NewDeployer(a.client, logger, egConfig, a.imageVector)

	tlsBundle, err := a.reconcileSecrets(ctx, logger, cluster, clusterName)
	if err != nil {
		return fmt.Errorf("failed to reconcile envoy-gateway TLS secrets: %w", err)
	}

	if err := deployer.Deploy(ctx, clusterName, tlsBundle); err != nil {
		return fmt.Errorf("failed to deploy envoy-gateway: %w", err)
	}

	logger.Info("successfully reconciled envoy-gateway extension", "cluster", clusterName)

	return nil
}

// reconcileSecrets generates (or rotates) the envoy-gateway CA and its xDS
// server cert via Gardener's secrets-manager, returns a [envoygateway.TLSBundle]
// ready to ship into the shoot, and runs the manager's cleanup so old
// generations are GC'd.
func (a *Actuator) reconcileSecrets(
	ctx context.Context,
	logger logr.Logger,
	cluster *extensionscontroller.Cluster,
	clusterName string,
) (envoygateway.TLSBundle, error) {
	sm, err := extensionssecretmanager.SecretsManagerForCluster(
		ctx,
		logger.WithName("secrets-manager"),
		clock.RealClock{},
		a.client,
		cluster,
		ExtensionType,
		envoygateway.SecretConfigs(),
	)
	if err != nil {
		return envoygateway.TLSBundle{}, fmt.Errorf("failed to construct secrets-manager: %w", err)
	}

	generated, err := extensionssecretmanager.GenerateAllSecrets(ctx, sm, envoygateway.SecretConfigs())
	if err != nil {
		return envoygateway.TLSBundle{}, fmt.Errorf("failed to generate envoy-gateway secrets: %w", err)
	}

	caSecret := generated[envoygateway.CASecretName]
	serverSecret := generated[envoygateway.XDSServerCertSecretName]
	clientSecret := generated[envoygateway.EnvoyClientCertSecretName]
	if caSecret == nil || serverSecret == nil || clientSecret == nil {
		return envoygateway.TLSBundle{}, fmt.Errorf("secrets-manager returned an incomplete secret set for %q", clusterName)
	}

	bundle := envoygateway.TLSBundle{
		CertPEM:       serverSecret.Data[secretsutils.DataKeyCertificate],
		KeyPEM:        serverSecret.Data[secretsutils.DataKeyPrivateKey],
		CAPEM:         caSecret.Data[secretsutils.DataKeyCertificateCA],
		ClientCertPEM: clientSecret.Data[secretsutils.DataKeyCertificate],
		ClientKeyPEM:  clientSecret.Data[secretsutils.DataKeyPrivateKey],
		HMACBytes:     envoygateway.HMACSecretBytes(clusterName),
	}
	if len(bundle.CertPEM) == 0 || len(bundle.KeyPEM) == 0 || len(bundle.CAPEM) == 0 ||
		len(bundle.ClientCertPEM) == 0 || len(bundle.ClientKeyPEM) == 0 {
		return envoygateway.TLSBundle{}, fmt.Errorf("secrets-manager returned an incomplete TLS bundle for %q", clusterName)
	}

	if err := sm.Cleanup(ctx); err != nil {
		return envoygateway.TLSBundle{}, fmt.Errorf("failed to clean up old envoy-gateway secrets: %w", err)
	}

	return bundle, nil
}

// Delete removes Envoy Gateway from the shoot cluster.
//
// Because lifecycle.delete is BeforeKubeAPIServer, the shoot API server is
// still reachable; resource-manager cleanly removes the shoot objects.
//
// When the shoot itself is *not* being deleted (the user disabled or removed
// the extension on a live shoot), we refuse to proceed while user-owned
// Gateway objects still exist in the shoot — pulling the extension out
// underneath live Gateways would silently lose traffic. When the entire shoot
// is being deleted, the guard is bypassed: blocking would only leak the shoot
// and the LB Services are cleaned up by the cloud-provider extension anyway.
func (a *Actuator) Delete(ctx context.Context, logger logr.Logger, ex *extensionsv1alpha1.Extension) (retErr error) {
	clusterName := ex.Namespace
	start := time.Now()
	defer func() {
		recordOperation(clusterName, "delete", start, retErr)
	}()

	logger.Info("deleting envoy-gateway resources managed by extension", "cluster", clusterName)

	cluster, err := extensionscontroller.GetCluster(ctx, a.client, clusterName)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	if err := a.checkNoUserGateways(ctx, logger, cluster.Shoot, clusterName); err != nil {
		return err
	}

	deployer := envoygateway.NewDeployer(a.client, logger, envoygateway.DefaultConfig(), a.imageVector)
	if err := deployer.Delete(ctx, clusterName); err != nil {
		return fmt.Errorf("failed to delete envoy-gateway: %w", err)
	}

	if err := a.deleteSecrets(ctx, logger, cluster); err != nil {
		return fmt.Errorf("failed to delete envoy-gateway secrets: %w", err)
	}

	logger.Info("successfully deleted envoy-gateway resources", "cluster", clusterName)

	return nil
}

// deleteSecrets garbage-collects the seed-side secrets generated by the
// secrets-manager for this extension. We call Cleanup on a manager that
// declares no configs, so every secret previously owned by this identity is
// removed.
func (a *Actuator) deleteSecrets(
	ctx context.Context,
	logger logr.Logger,
	cluster *extensionscontroller.Cluster,
) error {
	sm, err := extensionssecretmanager.SecretsManagerForCluster(
		ctx,
		logger.WithName("secrets-manager"),
		clock.RealClock{},
		a.client,
		cluster,
		ExtensionType,
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to construct secrets-manager: %w", err)
	}

	return sm.Cleanup(ctx)
}

// checkNoUserGateways is the live-Gateway delete guard from GEP-68. It runs
// before the actuator removes the ManagedResource and refuses the operation
// when user-owned Gateway objects still exist in the shoot — unless the entire
// shoot is being deleted, in which case the guard is bypassed (blocking would
// only leak the shoot, and LB Services are cleaned up by the cloud-provider
// extension anyway).
func (a *Actuator) checkNoUserGateways(ctx context.Context, logger logr.Logger, shoot *gardencorev1beta1.Shoot, clusterName string) error {
	if shoot != nil && shoot.DeletionTimestamp != nil {
		return nil
	}

	names, err := a.gatewayLister.ListGateways(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("failed to list Gateway resources in shoot before deleting extension: %w", err)
	}

	if len(names) == 0 {
		return nil
	}

	logger.Info("refusing to delete envoy-gateway extension: user Gateways still exist",
		"cluster", clusterName, "count", len(names))
	metrics.DeleteGuardRejectionsTotal.WithLabelValues(clusterName).Inc()

	return &gatewaysInUseError{names: names}
}

// ForceDelete deletes the ManagedResource without waiting for shoot-side cleanup.
func (a *Actuator) ForceDelete(ctx context.Context, logger logr.Logger, ex *extensionsv1alpha1.Extension) (retErr error) {
	clusterName := ex.Namespace
	start := time.Now()
	defer func() {
		recordOperation(clusterName, "force_delete", start, retErr)
	}()

	logger.Info("shoot has been force-deleted, deleting envoy-gateway resources", "cluster", clusterName)
	deployer := envoygateway.NewDeployer(a.client, logger, envoygateway.DefaultConfig(), a.imageVector)
	if err := deployer.DeleteKeepingObjects(ctx, clusterName); err != nil {
		return fmt.Errorf("failed to force-delete envoy-gateway: %w", err)
	}

	return nil
}

// Restore restores the resources managed by the extension actuator.
func (a *Actuator) Restore(ctx context.Context, logger logr.Logger, ex *extensionsv1alpha1.Extension) (retErr error) {
	start := time.Now()
	defer func() {
		recordOperation(ex.Namespace, "restore", start, retErr)
	}()

	return a.Reconcile(ctx, logger, ex)
}

// Migrate cleans up control-plane resources during a shoot control-plane migration.
// Shoot-side objects are preserved (keepObjects=true) so the new seed can
// recreate the ManagedResource.
func (a *Actuator) Migrate(ctx context.Context, logger logr.Logger, ex *extensionsv1alpha1.Extension) (retErr error) {
	clusterName := ex.Namespace
	start := time.Now()
	defer func() {
		recordOperation(clusterName, "migrate", start, retErr)
	}()

	logger.Info("migrating envoy-gateway extension, cleaning up control-plane resources", "cluster", clusterName)
	deployer := envoygateway.NewDeployer(a.client, logger, envoygateway.DefaultConfig(), a.imageVector)
	if err := deployer.DeleteKeepingObjects(ctx, clusterName); err != nil {
		return fmt.Errorf("failed to delete envoy-gateway managed resource during migrate: %w", err)
	}
	logger.Info("successfully migrated envoy-gateway extension", "cluster", clusterName)

	return nil
}

// applyLogLevel validates a configured log level and, when set, writes it to
// the target. An empty level is left untouched so the deployer's default
// ("info") stands.
func applyLogLevel(level config.LogLevel, target *string) error {
	if level == "" {
		return nil
	}
	if _, ok := envoygateway.ValidLogLevels[string(level)]; !ok {
		return fmt.Errorf("invalid envoy-gateway log level %q: must be one of debug, info, warn, error", level)
	}
	*target = string(level)

	return nil
}

// recordOperation writes the standard actuator metrics for a single lifecycle
// operation. Called from a deferred closure in every actuator method so total,
// duration, and (on failure) the classified error counter stay in one place.
func recordOperation(cluster, op string, start time.Time, err error) {
	metrics.ActuatorOperationTotal.WithLabelValues(cluster, op).Inc()
	metrics.ActuatorOperationDurationSeconds.WithLabelValues(cluster, op).Set(time.Since(start).Seconds())
	if err != nil {
		metrics.ActuatorOperationErrorsTotal.WithLabelValues(cluster, op, classifyError(err)).Inc()
	}
}

// classifyError maps an actuator error to a small, bounded set of reason
// labels for Prometheus. Cardinality must stay low: the metric label is meant
// for alert routing, not for search. Anything not matched falls into "other".
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if _, ok := errors.AsType[*gatewaysInUseError](err); ok {
		return metrics.ReasonGatewaysInUse
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to decode envoy-gateway providerConfig"),
		strings.Contains(msg, "invalid envoy-gateway log level"),
		strings.Contains(msg, "invalid envoy-gateway channel"):
		return metrics.ReasonInvalidProviderConfig
	case strings.Contains(msg, "envoy-gateway TLS secrets"),
		strings.Contains(msg, "envoy-gateway secrets"),
		strings.Contains(msg, "secrets-manager"):
		return metrics.ReasonSecrets
	case strings.Contains(msg, "failed to deploy envoy-gateway"),
		strings.Contains(msg, "failed to delete envoy-gateway"),
		strings.Contains(msg, "failed to force-delete envoy-gateway"):
		return metrics.ReasonManagedResource
	case strings.Contains(msg, "failed to get cluster"):
		return metrics.ReasonClusterLookup
	default:
		return metrics.ReasonOther
	}
}
