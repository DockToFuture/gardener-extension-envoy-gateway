// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"crypto/sha256"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BackendTarget identifies a route backend the data-plane proxies must be
// allowed to reach: the namespace the backend lives in and the pod selector
// copied from the backing Service's spec.selector.
type BackendTarget struct {
	// Namespace is the namespace the backend pods live in. For a same-namespace
	// backendRef this equals the Gateway namespace.
	Namespace string
	// PodSelector is a copy of the backing Service's spec.selector, used to
	// select the backend pods in NetworkPolicy peers.
	PodSelector map[string]string
}

// proxyPodSelector returns the label selector matching the data-plane Envoy
// proxy pods envoy-gateway provisions.
func proxyPodSelector() metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelManagedBy: EnvoyProxyManagedByValue,
			LabelName:      EnvoyProxyNameValue,
		},
	}
}

// managedLabels returns the labels the extension stamps on every data-plane
// NetworkPolicy it manages for the given hop. LabelName carries the policy's
// name so the control-plane NetworkPolicy and the three data-plane hops stay
// individually selectable; LabelHop scopes a prune to a single hop.
func managedLabels(name, hop string) map[string]string {
	return map[string]string{
		LabelManagedBy: LabelManagedByValue,
		LabelName:      name,
		LabelHop:       hop,
	}
}

// ManagedDataPlanePolicyLabels is the label selector matching every data-plane
// NetworkPolicy this extension manages, across all three hops. It scopes a
// prune List to policies this extension owns, regardless of hop.
func ManagedDataPlanePolicyLabels() client.MatchingLabels {
	return client.MatchingLabels{LabelManagedBy: LabelManagedByValue}
}

// ManagedHopPolicyLabels is the label selector matching the data-plane
// NetworkPolicies this extension manages for a single hop. It scopes a prune
// List so one hop's reconciliation never removes another hop's policies.
func ManagedHopPolicyLabels(hop string) client.MatchingLabels {
	return client.MatchingLabels{
		LabelManagedBy: LabelManagedByValue,
		LabelHop:       hop,
	}
}

// DataPlaneProxyIngressPolicy returns the hop-1 (client → proxy) ingress
// NetworkPolicy for a single Gateway namespace. It selects the Envoy data-plane
// proxy pods and allows ingress on the data-plane ports from anywhere: the
// extension does not know a Gateway's load-balancer scope (internal vs.
// internet-facing is a user-set Service annotation), and an internal-LB proxy
// is already unreachable from outside at the network layer, so an empty from
// grants no extra exposure.
func DataPlaneProxyIngressPolicy(namespace string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataPlaneNetworkPolicyName,
			Namespace: namespace,
			Labels:    managedLabels(DataPlaneNetworkPolicyName, HopProxyIngress),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: proxyPodSelector(),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: ptrIntStr(DataPlaneHTTPPort)},
					{Protocol: &tcp, Port: ptrIntStr(DataPlaneHTTPSPort)},
					{Protocol: &tcp, Port: ptrIntStr(DataPlaneReadyPort)},
				},
			}},
		},
	}
}

// ProxyEgressPolicy returns the hop-2 (proxy → backend) egress NetworkPolicy
// for a single Gateway namespace. It selects the data-plane proxy pods and
// allows egress to DNS (so the proxies can resolve Service names) plus one rule
// per resolved backend target. Ports are left open to the selected backend pods
// to match the documented "egress: - {}" posture — a route can target any
// backend container port and the extension does not track per-route ports.
func ProxyEgressPolicy(gatewayNamespace string, backends []BackendTarget) *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dnsPort := ptrIntStr(DataPlaneDNSPort)

	// Two fixed rules (DNS + xDS) plus one per backend.
	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, 2+len(backends))
	egress = append(egress,
		networkingv1.NetworkPolicyEgressRule{
			// DNS resolution to any namespace (kube-dns / node-local DNS).
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: dnsPort},
				{Protocol: &tcp, Port: dnsPort},
			},
		},
		networkingv1.NetworkPolicyEgressRule{
			// xDS: the proxy pulls its listener/cluster config from the Envoy Gateway
			// control-plane pod in kube-system. Without this, the gRPC xDS stream
			// times out, the proxy never programs a listener, and its readiness port
			// (19003) never opens, so the pod stays NotReady. The control plane's own
			// ingress policy allows this connection in (see Deployer.networkPolicy);
			// both sides are required under a default-deny posture.
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: namespaceByName(Namespace),
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					LabelName:     DeploymentName,
					LabelInstance: DeploymentName,
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: ptrIntStr(XDSPort)},
			},
		},
	)

	for _, b := range backends {
		peer := networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{MatchLabels: b.PodSelector},
		}
		// Scope a cross-namespace backend to its own namespace; a same-namespace
		// backend needs no namespaceSelector (defaults to the policy's namespace).
		if b.Namespace != gatewayNamespace {
			peer.NamespaceSelector = namespaceByName(b.Namespace)
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{peer},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyEgressNetworkPolicyName,
			Namespace: gatewayNamespace,
			Labels:    managedLabels(ProxyEgressNetworkPolicyName, HopProxyEgress),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: proxyPodSelector(),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// BackendIngressPolicy returns the hop-3 (backend ← proxy) ingress
// NetworkPolicy written into a backend namespace. It selects the backend pods
// (via the copied Service selector) and allows ingress from the data-plane
// proxy pods in the given Gateway namespace. The name embeds a short hash of
// the Gateway namespace so several Gateways targeting one backend namespace
// produce distinct, independently-pruneable policies.
func BackendIngressPolicy(backendNamespace, gatewayNamespace string, podSelector map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BackendIngressPolicyName(gatewayNamespace),
			Namespace: backendNamespace,
			Labels:    managedLabels(BackendIngressNetworkPolicyPrefix, HopBackendIngress),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceByName(gatewayNamespace),
					PodSelector:       new(proxyPodSelector()),
				}},
			}},
		},
	}
}

// BackendIngressPolicyName returns the deterministic name of the hop-3 policy
// for proxies originating in the given Gateway namespace. The 8-hex-char suffix
// keeps the name DNS-safe and collision-resistant across Gateway namespaces.
func BackendIngressPolicyName(gatewayNamespace string) string {
	sum := sha256.Sum256([]byte(gatewayNamespace))

	return BackendIngressNetworkPolicyPrefix + "-" + hex.EncodeToString(sum[:])[:8]
}

// StalePolicies returns the existing managed policies whose (namespace, name)
// is not in the desired set. Pure so the prune decision is unit-testable. A
// policy is keyed by both namespace and name because a single backend namespace
// may legitimately hold several hop-3 policies, one per originating Gateway
// namespace.
func StalePolicies(existing []networkingv1.NetworkPolicy, desired []client.ObjectKey) []networkingv1.NetworkPolicy {
	keep := make(map[client.ObjectKey]struct{}, len(desired))
	for _, k := range desired {
		keep[k] = struct{}{}
	}

	var stale []networkingv1.NetworkPolicy
	for _, p := range existing {
		if _, ok := keep[client.ObjectKey{Namespace: p.Namespace, Name: p.Name}]; !ok {
			stale = append(stale, p)
		}
	}

	return stale
}

// namespaceByName returns a namespace selector matching exactly the namespace
// with the given name via the well-known metadata.name label.
func namespaceByName(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{MetadataNameLabel: name}}
}

// ptrIntStr returns a pointer to an IntOrString holding the given int.
func ptrIntStr(port int) *intstr.IntOrString {
	v := intstr.FromInt(port)

	return &v
}
