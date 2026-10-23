// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
// name so the control-plane NetworkPolicy and the data-plane hops stay
// individually selectable; LabelHop scopes a prune to a single hop.
func managedLabels(name string, hop Hop) map[string]string {
	return map[string]string{
		LabelManagedBy: LabelManagedByValue,
		LabelName:      name,
		LabelHop:       string(hop),
	}
}

// ManagedDataPlanePolicyListOptions returns the List options matching every
// data-plane NetworkPolicy this extension manages, regardless of hop. The
// managed-by value ("gardener") is shared across Gardener components, so a
// prune must not key on it alone; requiring the presence of the hop label key
// — which only this extension stamps on its data-plane policies — scopes the
// selection to policies this extension owns.
func ManagedDataPlanePolicyListOptions() []client.ListOption {
	return []client.ListOption{
		client.MatchingLabels{LabelManagedBy: LabelManagedByValue},
		client.HasLabels{LabelHop},
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

// ProxyEgressPolicy returns the hop-2 (proxy egress) NetworkPolicy for a single
// Gateway namespace. It selects the data-plane proxy pods and allows egress to
// DNS (so the proxies can resolve names) and to the Envoy Gateway control plane
// (xDS). Both rules are required for the proxy to boot under a default-deny
// posture, independent of any route: without DNS it cannot resolve, and without
// xDS it never programs a listener and its readiness port never opens, so the
// pod stays NotReady. Egress to route backends is managed separately.
func ProxyEgressPolicy(gatewayNamespace string) *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dnsServicePort := ptrIntStr(DataPlaneDNSServicePort)
	dnsServerPort := ptrIntStr(DataPlaneDNSServerPort)

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			// DNS resolution to any namespace (kube-dns / node-local DNS). Open
			// both the Service port (53, node-local-dns) and the CoreDNS container
			// port (8053) because NetworkPolicy filters on the endpoint port after
			// Service translation — 53 alone fails on shoots without node-local-dns.
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: dnsServicePort},
				{Protocol: &tcp, Port: dnsServicePort},
				{Protocol: &udp, Port: dnsServerPort},
				{Protocol: &tcp, Port: dnsServerPort},
			},
		},
		{
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
