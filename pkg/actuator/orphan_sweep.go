// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"fmt"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// OrphanDataPlaneSweeper deletes the per-Gateway data-plane proxy resources that
// envoy-gateway's pre-GatewayNamespace deploy mode left behind in kube-system.
type OrphanDataPlaneSweeper interface {
	// Sweep deletes any orphaned data-plane proxy objects from kube-system in the
	// shoot identified by seedNamespace. It is idempotent: once the orphans are
	// gone it deletes nothing.
	Sweep(ctx context.Context, logger logr.Logger, seedNamespace string) error
}

// orphanDataPlaneSweeper talks to the shoot API server via a client built from
// the seed
type orphanDataPlaneSweeper struct {
	seedClient client.Client
}

// NewOrphanDataPlaneSweeper returns a sweeper that reaches the shoot API server
func NewOrphanDataPlaneSweeper(seedClient client.Client) OrphanDataPlaneSweeper {
	return &orphanDataPlaneSweeper{seedClient: seedClient}
}

func (s *orphanDataPlaneSweeper) Sweep(ctx context.Context, logger logr.Logger, seedNamespace string) error {
	scheme, err := kubernetesScheme()
	if err != nil {
		return err
	}

	_, shootClient, err := extensionsutil.NewClientForShoot(
		ctx,
		s.seedClient,
		seedNamespace,
		client.Options{Scheme: scheme},
		extensionsconfigv1alpha1.RESTOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to build shoot client for orphaned data-plane sweep: %w", err)
	}

	return sweepOrphans(ctx, logger, shootClient)
}

// sweepOrphans deletes the orphaned data-plane objects from kube-system using the
// given shoot client. Split out from Sweep so the delete loop can be tested
// against a fake client without a real shoot connection.
func sweepOrphans(ctx context.Context, logger logr.Logger, shootClient client.Client) error {
	// Each entry is a fresh, empty list of one kind. We List every object in
	// kube-system carrying envoy-gateway's managed-by label, then delete only
	// those that also carry an owning-gateway-name label — i.e. the per-Gateway
	// data-plane infra, never the control plane (which has no owning-gateway-* label).
	lists := []client.ObjectList{
		&corev1.ServiceList{},
		&appsv1.DeploymentList{},
		&corev1.ServiceAccountList{},
		&corev1.ConfigMapList{},
		&policyv1.PodDisruptionBudgetList{},
		&autoscalingv2.HorizontalPodAutoscalerList{},
	}

	listOpts := []client.ListOption{
		client.InNamespace(envoygateway.Namespace),
		client.MatchingLabels{envoygateway.LabelManagedBy: envoygateway.EnvoyProxyManagedByValue},
	}

	var deleted int
	for _, list := range lists {
		if err := shootClient.List(ctx, list, listOpts...); err != nil {
			return fmt.Errorf("failed to list orphaned data-plane objects in %q: %w", envoygateway.Namespace, err)
		}

		objs, err := meta.ExtractList(list)
		if err != nil {
			return fmt.Errorf("failed to extract orphaned data-plane object list: %w", err)
		}

		for _, o := range objs {
			obj, ok := o.(client.Object)
			if !ok {
				continue
			}
			if !isOrphanedDataPlaneObject(obj) {
				continue
			}

			if err := shootClient.Delete(ctx, obj); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}

				return fmt.Errorf("failed to delete orphaned data-plane %T %s/%s: %w",
					obj, obj.GetNamespace(), obj.GetName(), err)
			}

			deleted++
			logger.Info("deleted orphaned data-plane resource left by pre-GatewayNamespace deploy mode",
				"kind", fmt.Sprintf("%T", obj), "namespace", obj.GetNamespace(), "name", obj.GetName(),
				"owningGateway", obj.GetLabels()[envoygateway.OwningGatewayNamespaceLabel]+"/"+obj.GetLabels()[envoygateway.OwningGatewayNameLabel])
		}
	}

	if deleted > 0 {
		logger.Info("swept orphaned data-plane resources from kube-system", "count", deleted)
	}

	return nil
}

// isOrphanedDataPlaneObject reports whether obj is one of envoy-gateway's
// per-Gateway data-plane infra objects — i.e. it carries the owning-gateway-name
// label. The extension's own control-plane resources in kube-system never carry
// that label, so this is the boundary that keeps the sweep from deleting the
// control plane.
func isOrphanedDataPlaneObject(obj client.Object) bool {
	_, ok := obj.GetLabels()[envoygateway.OwningGatewayNameLabel]

	return ok
}
