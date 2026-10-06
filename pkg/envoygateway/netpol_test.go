// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nsTeamA = "team-a"
	nsTeamB = "team-b"

	testAppLabel    = "app"
	testBackendName = "backend"
)

func TestDataPlaneProxyIngressPolicy_Spec(t *testing.T) {
	np := DataPlaneProxyIngressPolicy(nsTeamA)

	if np.Name != DataPlaneNetworkPolicyName {
		t.Errorf("expected policy name %q, got %q", DataPlaneNetworkPolicyName, np.Name)
	}
	if np.Namespace != nsTeamA {
		t.Errorf("expected namespace %q, got %q", nsTeamA, np.Namespace)
	}

	if got := np.Spec.PodSelector.MatchLabels[LabelManagedBy]; got != EnvoyProxyManagedByValue {
		t.Errorf("expected podSelector managed-by=%q, got %q", EnvoyProxyManagedByValue, got)
	}
	if got := np.Spec.PodSelector.MatchLabels[LabelName]; got != EnvoyProxyNameValue {
		t.Errorf("expected podSelector name=%q, got %q", EnvoyProxyNameValue, got)
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
	for _, want := range []int32{DataPlaneHTTPPort, DataPlaneHTTPSPort, DataPlaneReadyPort} {
		if !gotPorts[want] {
			t.Errorf("expected ingress port %d, got %v", want, gotPorts)
		}
	}

	if got := np.Labels[LabelManagedBy]; got != LabelManagedByValue {
		t.Errorf("expected label managed-by=%q, got %q", LabelManagedByValue, got)
	}
	if got := np.Labels[LabelName]; got != DataPlaneNetworkPolicyName {
		t.Errorf("expected label name=%q, got %q", DataPlaneNetworkPolicyName, got)
	}
	if got := np.Labels[LabelHop]; got != HopProxyIngress {
		t.Errorf("expected hop label %q, got %q", HopProxyIngress, got)
	}
}

func TestProxyEgressPolicy_SameAndCrossNamespaceBackends(t *testing.T) {
	backends := []BackendTarget{
		{Namespace: nsTeamA, PodSelector: map[string]string{testAppLabel: "local"}},
		{Namespace: nsTeamB, PodSelector: map[string]string{testAppLabel: "remote"}},
	}
	np := ProxyEgressPolicy(nsTeamA, backends)

	if np.Name != ProxyEgressNetworkPolicyName {
		t.Errorf("expected policy name %q, got %q", ProxyEgressNetworkPolicyName, np.Name)
	}
	if np.Namespace != nsTeamA {
		t.Errorf("expected namespace %q, got %q", nsTeamA, np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("expected a single Egress policy type, got %v", np.Spec.PolicyTypes)
	}
	if got := np.Labels[LabelHop]; got != HopProxyEgress {
		t.Errorf("expected hop label %q, got %q", HopProxyEgress, got)
	}

	// First rule is DNS: a namespaceSelector peer with UDP+TCP 53.
	// Second rule is xDS egress to the control plane. Then one rule per backend.
	if len(np.Spec.Egress) != 4 {
		t.Fatalf("expected 4 egress rules (DNS + xDS + 2 backends), got %d", len(np.Spec.Egress))
	}
	dns := np.Spec.Egress[0]
	if len(dns.Ports) != 2 {
		t.Errorf("expected 2 DNS ports (UDP+TCP), got %d", len(dns.Ports))
	}
	for _, p := range dns.Ports {
		if p.Port == nil || p.Port.IntVal != DataPlaneDNSPort {
			t.Errorf("expected DNS port %d, got %v", DataPlaneDNSPort, p.Port)
		}
	}

	// xDS rule: egress to the control-plane pod in kube-system on the xDS port.
	xds := np.Spec.Egress[1]
	if len(xds.Ports) != 1 || xds.Ports[0].Port == nil || xds.Ports[0].Port.IntVal != XDSPort {
		t.Errorf("expected a single xDS egress port %d, got %v", XDSPort, xds.Ports)
	}
	if xds.To[0].NamespaceSelector == nil || xds.To[0].NamespaceSelector.MatchLabels[MetadataNameLabel] != Namespace {
		t.Errorf("expected xDS peer scoped to namespace %q, got %v", Namespace, xds.To[0].NamespaceSelector)
	}
	if got := xds.To[0].PodSelector.MatchLabels[LabelName]; got != DeploymentName {
		t.Errorf("expected xDS peer podSelector %s=%q, got %q", LabelName, DeploymentName, got)
	}

	// Same-namespace backend: no namespaceSelector.
	local := np.Spec.Egress[2]
	if local.To[0].NamespaceSelector != nil {
		t.Error("expected same-namespace backend peer to omit a namespaceSelector")
	}
	if got := local.To[0].PodSelector.MatchLabels[testAppLabel]; got != "local" {
		t.Errorf("expected same-ns podSelector app=local, got %q", got)
	}

	// Cross-namespace backend: namespaceSelector scoped to the backend namespace.
	remote := np.Spec.Egress[3]
	if remote.To[0].NamespaceSelector == nil {
		t.Fatal("expected cross-namespace backend peer to carry a namespaceSelector")
	}
	if got := remote.To[0].NamespaceSelector.MatchLabels[MetadataNameLabel]; got != nsTeamB {
		t.Errorf("expected cross-ns namespaceSelector metadata.name=%q, got %q", nsTeamB, got)
	}
	if got := remote.To[0].PodSelector.MatchLabels[testAppLabel]; got != "remote" {
		t.Errorf("expected cross-ns podSelector app=remote, got %q", got)
	}
}

func TestBackendIngressPolicy_Spec(t *testing.T) {
	np := BackendIngressPolicy(nsTeamB, nsTeamA, map[string]string{testAppLabel: testBackendName})

	if np.Namespace != nsTeamB {
		t.Errorf("expected policy in backend namespace %q, got %q", nsTeamB, np.Namespace)
	}
	if np.Name != BackendIngressPolicyName(nsTeamA) {
		t.Errorf("expected name %q, got %q", BackendIngressPolicyName(nsTeamA), np.Name)
	}
	if got := np.Spec.PodSelector.MatchLabels[testAppLabel]; got != testBackendName {
		t.Errorf("expected podSelector app=backend, got %q", got)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("expected a single Ingress policy type, got %v", np.Spec.PolicyTypes)
	}
	if got := np.Labels[LabelHop]; got != HopBackendIngress {
		t.Errorf("expected hop label %q, got %q", HopBackendIngress, got)
	}

	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels[MetadataNameLabel] != nsTeamA {
		t.Errorf("expected ingress peer scoped to Gateway namespace %q, got %v", nsTeamA, peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels[LabelName] != EnvoyProxyNameValue {
		t.Errorf("expected ingress peer to select proxy pods, got %v", peer.PodSelector)
	}
}

func TestBackendIngressPolicyName_DistinctPerGatewayNamespace(t *testing.T) {
	a := BackendIngressPolicyName(nsTeamA)
	b := BackendIngressPolicyName(nsTeamB)
	if a == b {
		t.Fatalf("expected distinct names for distinct Gateway namespaces, both %q", a)
	}
	if a != BackendIngressPolicyName(nsTeamA) {
		t.Error("expected the name to be deterministic for the same Gateway namespace")
	}
}

func policyIn(ns, name, hop string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    managedLabels(name, hop),
		},
	}
}

func TestStalePolicies_KeepsDesiredDeletesRest(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policyIn(nsTeamA, DataPlaneNetworkPolicyName, HopProxyIngress),
		policyIn(nsTeamB, DataPlaneNetworkPolicyName, HopProxyIngress),
		policyIn("orphan", DataPlaneNetworkPolicyName, HopProxyIngress),
	}

	stale := StalePolicies(existing, []client.ObjectKey{
		{Namespace: nsTeamA, Name: DataPlaneNetworkPolicyName},
		{Namespace: nsTeamB, Name: DataPlaneNetworkPolicyName},
	})
	if len(stale) != 1 {
		t.Fatalf("expected exactly one stale policy, got %d: %v", len(stale), stale)
	}
	if stale[0].Namespace != "orphan" {
		t.Errorf("expected the orphan namespace to be pruned, got %q", stale[0].Namespace)
	}
}

func TestStalePolicies_EmptyDesiredDeletesAll(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policyIn(nsTeamA, DataPlaneNetworkPolicyName, HopProxyIngress),
		policyIn(nsTeamB, DataPlaneNetworkPolicyName, HopProxyIngress),
	}

	if stale := StalePolicies(existing, nil); len(stale) != len(existing) {
		t.Fatalf("expected every policy to be stale with an empty desired set, got %d of %d", len(stale), len(existing))
	}
}

// Two hop-3 policies in one backend namespace, one per originating Gateway
// namespace, must be keyed independently: dropping one Gateway namespace must
// prune only its policy, not the sibling that shares the backend namespace.
func TestStalePolicies_DistinctNamesInOneNamespace(t *testing.T) {
	nameA := BackendIngressPolicyName(nsTeamA)
	nameB := BackendIngressPolicyName(nsTeamB)
	existing := []networkingv1.NetworkPolicy{
		policyIn(testBackendName, nameA, HopBackendIngress),
		policyIn(testBackendName, nameB, HopBackendIngress),
	}

	stale := StalePolicies(existing, []client.ObjectKey{{Namespace: testBackendName, Name: nameA}})
	if len(stale) != 1 || stale[0].Name != nameB {
		t.Fatalf("expected only the %q policy to be pruned, got %v", nameB, stale)
	}
}
