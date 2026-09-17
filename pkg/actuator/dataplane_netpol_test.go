// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

func TestDataPlaneNetworkPolicy_Spec(t *testing.T) {
	np := dataPlaneNetworkPolicy("team-a")

	if np.Name != envoygateway.DataPlaneNetworkPolicyName {
		t.Errorf("expected policy name %q, got %q", envoygateway.DataPlaneNetworkPolicyName, np.Name)
	}
	if np.Namespace != "team-a" {
		t.Errorf("expected namespace %q, got %q", "team-a", np.Namespace)
	}

	if got := np.Spec.PodSelector.MatchLabels[envoygateway.LabelManagedBy]; got != envoygateway.EnvoyProxyManagedByValue {
		t.Errorf("expected podSelector managed-by=%q, got %q", envoygateway.EnvoyProxyManagedByValue, got)
	}
	if got := np.Spec.PodSelector.MatchLabels[envoygateway.LabelName]; got != envoygateway.EnvoyProxyNameValue {
		t.Errorf("expected podSelector name=%q, got %q", envoygateway.EnvoyProxyNameValue, got)
	}

	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("expected a single Ingress policy type, got %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected exactly one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 0 {
		t.Errorf("expected an empty From (allow from anywhere), got %v", rule.From)
	}
	gotPorts := map[int32]bool{}
	for _, p := range rule.Ports {
		if p.Port == nil {
			t.Fatal("expected a numeric port on the ingress rule")
		}
		gotPorts[p.Port.IntVal] = true
	}
	for _, want := range []int32{envoygateway.DataPlaneHTTPPort, envoygateway.DataPlaneHTTPSPort, envoygateway.DataPlaneReadyPort} {
		if !gotPorts[want] {
			t.Errorf("expected ingress port %d, got %v", want, gotPorts)
		}
	}

	if got := np.Labels[envoygateway.LabelManagedBy]; got != envoygateway.LabelManagedByValue {
		t.Errorf("expected label managed-by=%q, got %q", envoygateway.LabelManagedByValue, got)
	}
	if got := np.Labels[envoygateway.LabelName]; got != envoygateway.DataPlaneNetworkPolicyName {
		t.Errorf("expected label name=%q, got %q", envoygateway.DataPlaneNetworkPolicyName, got)
	}
}

func TestManagedDataPlanePolicyLabels_MatchBuilder(t *testing.T) {
	// The prune List selector must match exactly the labels the builder stamps,
	// or the reconciler would either miss its own policies or select foreign ones.
	labels := managedDataPlanePolicyLabels()
	built := dataPlaneNetworkPolicy("any").Labels

	for k, v := range labels {
		if built[k] != v {
			t.Errorf("prune selector label %q=%q not present on built policy (got %q)", k, v, built[k])
		}
	}
}

func policyIn(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envoygateway.DataPlaneNetworkPolicyName,
			Namespace: ns,
		},
	}
}

func TestStalePolicies_KeepsDesiredDeletesRest(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policyIn("team-a"),
		policyIn("team-b"),
		policyIn("orphan"),
	}

	stale := stalePolicies(existing, []string{"team-a", "team-b"})
	if len(stale) != 1 {
		t.Fatalf("expected exactly one stale policy, got %d: %v", len(stale), stale)
	}
	if stale[0].Namespace != "orphan" {
		t.Errorf("expected the orphan namespace to be pruned, got %q", stale[0].Namespace)
	}
}

func TestStalePolicies_EmptyDesiredDeletesAll(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policyIn("team-a"),
		policyIn("team-b"),
	}

	stale := stalePolicies(existing, nil)
	if len(stale) != len(existing) {
		t.Fatalf("expected every policy to be stale with an empty desired set, got %d of %d", len(stale), len(existing))
	}
}

func TestStalePolicies_AllDesiredNothingStale(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policyIn("team-a"),
		policyIn("team-b"),
	}

	if stale := stalePolicies(existing, []string{"team-a", "team-b"}); len(stale) != 0 {
		t.Errorf("expected no stale policies when all are desired, got %v", stale)
	}
}
