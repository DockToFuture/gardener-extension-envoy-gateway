// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

const (
	nsTeamA = "team-a"
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
	if got := np.Labels[LabelHop]; got != string(HopProxyIngress) {
		t.Errorf("expected hop label %q, got %q", HopProxyIngress, got)
	}
}

func TestProxyEgressPolicy_DNSAndXDSOnly(t *testing.T) {
	np := ProxyEgressPolicy(nsTeamA)

	if np.Name != ProxyEgressNetworkPolicyName {
		t.Errorf("expected policy name %q, got %q", ProxyEgressNetworkPolicyName, np.Name)
	}
	if np.Namespace != nsTeamA {
		t.Errorf("expected namespace %q, got %q", nsTeamA, np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("expected a single Egress policy type, got %v", np.Spec.PolicyTypes)
	}
	if got := np.Labels[LabelHop]; got != string(HopProxyEgress) {
		t.Errorf("expected hop label %q, got %q", HopProxyEgress, got)
	}
	if got := np.Spec.PodSelector.MatchLabels[LabelName]; got != EnvoyProxyNameValue {
		t.Errorf("expected podSelector to select proxy pods, got %v", np.Spec.PodSelector.MatchLabels)
	}

	// The policy carries exactly two rules — DNS and xDS — and no route-derived
	// backend rules. Backend egress is managed separately.
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("expected exactly 2 egress rules (DNS + xDS), got %d", len(np.Spec.Egress))
	}

	// First rule is DNS: a namespaceSelector peer with UDP+TCP on both the
	// Service port (53) and the CoreDNS container port (8053).
	dns := np.Spec.Egress[0]
	if len(dns.Ports) != 4 {
		t.Errorf("expected 4 DNS ports (UDP+TCP on 53 and 8053), got %d", len(dns.Ports))
	}
	for _, p := range dns.Ports {
		if p.Port == nil || (p.Port.IntVal != DataPlaneDNSServicePort && p.Port.IntVal != DataPlaneDNSServerPort) {
			t.Errorf("expected DNS port %d or %d, got %v", DataPlaneDNSServicePort, DataPlaneDNSServerPort, p.Port)
		}
	}

	// Second rule is xDS egress to the control-plane pod in kube-system on the
	// xDS port.
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
}
