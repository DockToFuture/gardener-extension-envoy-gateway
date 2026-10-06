// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"fmt"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// labelValueAllowed is the value Gardener's networking NetworkPolicies gate on.
const labelValueAllowed = "allowed"

// NetpolControllerDeployer manages the per-shoot data-plane NetworkPolicy
// controller: a single-replica Deployment that runs in the shoot's control-plane
// namespace on the seed and watches the shoot's Gateways/routes to reconcile the
// three-hop data-plane NetworkPolicies. It is deployed imperatively per Extension
// reconcile (there is no chart precedent for per-shoot seed Deployments), the
// same way the per-shoot TLS secrets are.
type NetpolControllerDeployer interface {
	// Ensure mints the controller's shoot-access secret and creates/updates its
	// seed Deployment in the control-plane namespace. experimental enables the
	// experimental-channel route watches.
	Ensure(ctx context.Context, logger logr.Logger, cluster *extensionscontroller.Cluster, seedNamespace string, experimental bool) error
	// Teardown deletes the Deployment and the shoot-access secret (IgnoreNotFound).
	Teardown(ctx context.Context, logger logr.Logger, seedNamespace string) error
	// ScaleDown patches the Deployment's replicas to zero (hibernation), leaving
	// the shoot-access secret in place so the controller resumes on wake-up.
	ScaleDown(ctx context.Context, logger logr.Logger, seedNamespace string) error
}

// netpolControllerDeployer is the default [NetpolControllerDeployer], writing
// against the seed via seedClient.
type netpolControllerDeployer struct {
	seedClient  client.Client
	imageVector imagevector.ImageVector
}

// NewNetpolControllerDeployer returns a deployer that creates the controller's
// resources in the seed control-plane namespace via seedClient.
func NewNetpolControllerDeployer(seedClient client.Client, imageVector imagevector.ImageVector) NetpolControllerDeployer {
	return &netpolControllerDeployer{seedClient: seedClient, imageVector: imageVector}
}

func (d *netpolControllerDeployer) Ensure(ctx context.Context, logger logr.Logger, cluster *extensionscontroller.Cluster, seedNamespace string, experimental bool) error {
	// Mint the shoot-access secret: the token-requestor controller of
	// gardener-resource-manager fills .data.token for the shoot ServiceAccount,
	// and the generic kubeconfig projected below references it.
	accessSecret := gardenerutils.NewShootAccessSecret(envoygateway.NetpolControllerShootAccessSecretName, seedNamespace).
		WithServiceAccountName(envoygateway.NetpolControllerServiceAccountName)
	if err := accessSecret.Reconcile(ctx, d.seedClient); err != nil {
		return fmt.Errorf("failed to reconcile netpol-controller shoot-access secret: %w", err)
	}

	deployment, err := d.deployment(seedNamespace, experimental)
	if err != nil {
		return err
	}

	// Project the generic shoot kubeconfig + the access token into the pod at
	// gardenerutils.PathGenericKubeconfig, so the subcommand can build the shoot
	// REST config from it.
	genericKubeconfigName := extensionscontroller.GenericTokenKubeconfigSecretNameFromCluster(cluster)
	if err := gardenerutils.InjectGenericKubeconfig(deployment, genericKubeconfigName, accessSecret.Secret.Name); err != nil {
		return fmt.Errorf("failed to inject generic kubeconfig into netpol-controller Deployment: %w", err)
	}

	obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.NetpolControllerName, Namespace: seedNamespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, d.seedClient, obj, func() error {
		obj.Labels = deployment.Labels
		obj.Spec = deployment.Spec

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to apply netpol-controller Deployment: %w", err)
	}

	logger.Info("ensured data-plane NetworkPolicy controller", "namespace", seedNamespace, "experimental", experimental)

	return nil
}

func (d *netpolControllerDeployer) Teardown(ctx context.Context, logger logr.Logger, seedNamespace string) error {
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.NetpolControllerName, Namespace: seedNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: gardenerutils.SecretNamePrefixShootAccess + envoygateway.NetpolControllerShootAccessSecretName, Namespace: seedNamespace}},
	}
	for _, obj := range objs {
		if err := d.seedClient.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete netpol-controller %T %s/%s: %w", obj, seedNamespace, obj.GetName(), err)
		}
	}

	logger.Info("tore down data-plane NetworkPolicy controller", "namespace", seedNamespace)

	return nil
}

func (d *netpolControllerDeployer) ScaleDown(ctx context.Context, logger logr.Logger, seedNamespace string) error {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.NetpolControllerName, Namespace: seedNamespace}}
	if err := d.seedClient.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		// Nothing to scale down if it was never deployed.
		return client.IgnoreNotFound(err)
	}

	patch := client.MergeFrom(deployment.DeepCopy())
	deployment.Spec.Replicas = ptr.To[int32](0)
	if err := d.seedClient.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("failed to scale down netpol-controller Deployment: %w", err)
	}

	logger.Info("scaled down data-plane NetworkPolicy controller", "namespace", seedNamespace)

	return nil
}

// deployment builds the controller's seed Deployment. The pod carries the
// gardener networking labels it needs to reach the shoot kube-apiserver (watch
// Gateways/routes), the seed runtime apiserver (its leader-election lease), and
// DNS. The container runs the `netpol-controller` subcommand of this extension's
// own image.
//
//nolint:revive // experimental is intrinsic config (which route kinds to watch), not control-flow coupling.
func (d *netpolControllerDeployer) deployment(seedNamespace string, experimental bool) (*appsv1.Deployment, error) {
	img, err := d.imageVector.FindImage(envoygateway.ExtensionImageName)
	if err != nil {
		return nil, fmt.Errorf("failed to find %s image in image vector: %w", envoygateway.ExtensionImageName, err)
	}

	labels := map[string]string{
		envoygateway.LabelName:      envoygateway.NetpolControllerName,
		envoygateway.LabelManagedBy: envoygateway.LabelManagedByValue,
	}
	podLabels := map[string]string{
		envoygateway.LabelName:                     envoygateway.NetpolControllerName,
		envoygateway.LabelManagedBy:                envoygateway.LabelManagedByValue,
		envoygateway.LabelToAllShootsKubeAPIServer: labelValueAllowed,
		envoygateway.LabelToDNS:                    labelValueAllowed,
		envoygateway.LabelToRuntimeAPIServer:       labelValueAllowed,
	}

	args := []string{
		"netpol-controller",
		"--seed-namespace=" + seedNamespace,
	}
	if experimental {
		args = append(args, "--experimental-channel=true")
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envoygateway.NetpolControllerName,
			Namespace: seedNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{envoygateway.LabelName: envoygateway.NetpolControllerName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    ptr.To[int64](65532),
						RunAsGroup:   ptr.To[int64](65532),
						FSGroup:      ptr.To[int64](65532),
					},
					Containers: []corev1.Container{{
						Name:  envoygateway.NetpolControllerName,
						Image: img.String(),
						Args:  args,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("20m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							ReadOnlyRootFilesystem:   new(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}, nil
}
