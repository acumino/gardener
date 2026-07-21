// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

// Package peerexposure implements a component that exposes the peer endpoints of a shoot's etcd members across seed
// clusters. It is used during a live control plane migration (GEP-39) to form a temporary etcd cluster whose members
// span the source and destination seed. The exposure is realized through the seed's shared Istio ingress gateway using
// TLS passthrough on the etcd peer port, routed to the correct member's headless peer Service via SNI. etcd peer
// traffic is already mutually authenticated with TLS, so the gateway must not terminate it.
package peerexposure

import (
	"context"
	"fmt"

	istioapinetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/component"
	etcdconstants "github.com/gardener/gardener/pkg/component/etcd/etcd/constants"
	"github.com/gardener/gardener/pkg/controllerutils"
	kubernetesutils "github.com/gardener/gardener/pkg/utils/kubernetes"
)

// etcdStatefulSetLabelAppValue is the value of the "app" label on the etcd pods managed by etcd-druid. It mirrors
// etcd.LabelAppValue and is duplicated here to avoid importing the heavy etcd component package.
const etcdStatefulSetLabelAppValue = "etcd-statefulset"

// IstioIngressGateway contains the values for the Istio ingress gateway that exposes the etcd peer endpoints.
type IstioIngressGateway struct {
	// Namespace is the namespace of the Istio ingress gateway.
	Namespace string
	// Labels are the selector labels of the Istio ingress gateway.
	Labels map[string]string
}

// PeerMember holds the cross-seed routing info for one etcd member.
type PeerMember struct {
	// SNIHost is the hostname under which this member is reachable from other seeds via the Istio ingress gateway.
	// It must appear in the member's peer TLS cert SANs.
	SNIHost string
	// PodFQDN is the in-cluster fully-qualified pod subdomain through the headless peer Service
	// (e.g. etcd-main-0.etcd-main-peer.<namespace>.svc.cluster.local). Incoming traffic matching SNIHost is
	// forwarded to exactly this pod, ensuring peer messages reach the correct member.
	PodFQDN string
	// ExternalPort is the port on the Istio ingress gateway LB at which this member is reachable. Each member is
	// assigned a unique port (etcdPeerBasePort + memberIndex) so that etcd's URLStringsEqual, which falls back to
	// DNS resolution, sees distinct IP:port tuples and cannot conflate members that share the same ingress IP.
	ExternalPort uint32
}

// Values are the configuration values for the etcd peer exposure component.
type Values struct {
	// Role is the role of the etcd (e.g. "main" or "events").
	Role string
	// Members is the per-member routing table: each entry maps one SNI hostname to the pod it must reach.
	// The Gateway and DestinationRule are built from the union of all SNI hosts; the VirtualService creates
	// one TLS route per member so traffic is never load-balanced to the wrong pod.
	Members []PeerMember
	// ClientHost is the SNI host under which this seed's etcd client endpoint is reachable from other seeds. It is
	// routed to the etcd client Service. When empty, no client exposure is deployed.
	ClientHost string
	// IstioIngressGateway is the Istio ingress gateway that exposes the peer endpoints.
	IstioIngressGateway IstioIngressGateway
}

// New creates a new instance of DeployWaiter which exposes the peer endpoints of a shoot's etcd members across seeds.
func New(cl client.Client, namespace string, values Values) component.DeployWaiter {
	return &peerExposure{
		client:    cl,
		namespace: namespace,
		values:    values,
	}
}

type peerExposure struct {
	client    client.Client
	namespace string
	values    Values
}

func (p *peerExposure) name() string {
	return fmt.Sprintf("etcd-%s-peer", p.values.Role)
}

func (p *peerExposure) Deploy(ctx context.Context) error {
	gateway := p.emptyGateway()
	virtualService := p.emptyVirtualService()
	networkPolicy := p.emptyNetworkPolicy()

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, gateway, gatewayWithPeerTLSPassthrough(gateway, getLabels(p.values.Role), p.values.IstioIngressGateway.Labels, p.values.Members)); err != nil {
		return fmt.Errorf("failed to reconcile Istio gateway for etcd peer exposure: %w", err)
	}

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, virtualService, virtualServiceWithPeerSNIMatch(virtualService, getLabels(p.values.Role), []string{p.values.IstioIngressGateway.Namespace}, p.values.Members, gateway.Name)); err != nil {
		return fmt.Errorf("failed to reconcile Istio virtual service for etcd peer exposure: %w", err)
	}

	// Gardener seeds set defaultServiceExportTo: ["~"] in the mesh config, so services are self-namespace only by
	// default. ServiceEntries with resolution: DNS and exportTo pointing at the istio-ingress namespace make each
	// pod's subdomain discoverable by the ingress proxy without touching the etcd-druid-owned Service (which the druid
	// admission webhook would reject). A DestinationRule exportTo only exports traffic policies, not endpoint
	// visibility — ServiceEntry is the correct primitive for endpoint export.
	// One ServiceEntry per pod is needed because headless-service pod subdomains are separate service
	// entries in Istio's registry and must be exported individually.
	for i, m := range p.values.Members {
		se := p.emptyPodServiceEntry(i)
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, se, serviceEntryForExport(se, getLabels(p.values.Role), m.PodFQDN, p.values.IstioIngressGateway.Namespace, uint32(etcdconstants.PortEtcdPeer), "tls-etcd-peer")); err != nil {
			return fmt.Errorf("failed to reconcile Istio service entry for etcd peer member %d: %w", i, err)
		}
	}

	// Allow the Istio ingress gateway (which terminates the cross-seed peer traffic on the load balancer and forwards
	// it to the local etcd member) to reach the etcd pods on the peer port. Two complementary NetworkPolicies are
	// needed because seeds have a deny-all policy in both the shoot namespace and the istio-ingress namespace:
	//   - networkPolicy (shoot namespace, Ingress): allows inbound peer traffic to etcd pods from istio gateway pods.
	//   - istioEgressNetworkPolicy (istio-ingress namespace, Egress): allows the istio gateway pods to open outbound
	//     connections to etcd pods. Without this egress rule the gateway proxy cannot forward TLS-passthrough traffic
	//     even though the etcd side already allows the ingress.
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, networkPolicy, func() error {
		networkPolicy.Labels = getLabels(p.values.Role)
		networkPolicy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				v1beta1constants.LabelApp:  etcdStatefulSetLabelAppValue,
				v1beta1constants.LabelRole: p.values.Role,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: p.values.IstioIngressGateway.Namespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: p.values.IstioIngressGateway.Labels},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Port:     new(intstr.FromInt32(etcdconstants.PortEtcdPeer)),
					Protocol: new(corev1.ProtocolTCP),
				}},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile network policy for etcd peer exposure: %w", err)
	}

	istioEgressNP := p.emptyIstioEgressNetworkPolicy()
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, istioEgressNP, func() error {
		istioEgressNP.Labels = getLabels(p.values.Role)
		istioEgressNP.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: p.values.IstioIngressGateway.Labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: p.namespace}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						v1beta1constants.LabelApp:  etcdStatefulSetLabelAppValue,
						v1beta1constants.LabelRole: p.values.Role,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Port:     new(intstr.FromInt32(etcdconstants.PortEtcdPeer)),
					Protocol: new(corev1.ProtocolTCP),
				}},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile Istio egress network policy for etcd peer exposure: %w", err)
	}

	if p.values.ClientHost != "" {
		if err := p.deployClientExposure(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (p *peerExposure) deployClientExposure(ctx context.Context) error {
	clientGateway := p.emptyClientGateway()
	clientVirtualService := p.emptyClientVirtualService()
	clientNetworkPolicy := p.emptyClientNetworkPolicy()

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, clientGateway, gatewayWithClientTLSPassthrough(clientGateway, getLabels(p.values.Role), p.values.IstioIngressGateway.Labels, []string{p.values.ClientHost})); err != nil {
		return fmt.Errorf("failed to reconcile Istio gateway for etcd client exposure: %w", err)
	}

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, clientVirtualService, virtualServiceWithClientSNIMatch(clientVirtualService, getLabels(p.values.Role), []string{p.values.IstioIngressGateway.Namespace}, []string{p.values.ClientHost}, clientGateway.Name, p.clientServiceHost())); err != nil {
		return fmt.Errorf("failed to reconcile Istio virtual service for etcd client exposure: %w", err)
	}

	// ServiceEntry (resolution: DNS) exports the client Service to the ingress proxy so that the VirtualService
	// can route the cross-seed client connection. A DestinationRule exportTo only exports traffic policies,
	// not endpoint visibility — ServiceEntry is the correct primitive here.
	clientSE := p.emptyClientServiceEntry()
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, clientSE, serviceEntryForExport(clientSE, getLabels(p.values.Role), p.clientServiceHost(), p.values.IstioIngressGateway.Namespace, uint32(etcdconstants.PortEtcdClient), "tls-etcd-client")); err != nil {
		return fmt.Errorf("failed to reconcile Istio service entry for etcd client exposure: %w", err)
	}

	// Allow the Istio ingress gateway to reach the etcd pods on the client port. Same two-NP pattern as
	// for the peer port: one ingress NP in the shoot namespace and one egress NP in the istio-ingress namespace.
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, clientNetworkPolicy, func() error {
		clientNetworkPolicy.Labels = getLabels(p.values.Role)
		clientNetworkPolicy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				v1beta1constants.LabelApp:  etcdStatefulSetLabelAppValue,
				v1beta1constants.LabelRole: p.values.Role,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: p.values.IstioIngressGateway.Namespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: p.values.IstioIngressGateway.Labels},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Port:     new(intstr.FromInt32(etcdconstants.PortEtcdClient)),
					Protocol: new(corev1.ProtocolTCP),
				}},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile network policy for etcd client exposure: %w", err)
	}

	clientIstioEgressNP := p.emptyClientIstioEgressNetworkPolicy()
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, p.client, clientIstioEgressNP, func() error {
		clientIstioEgressNP.Labels = getLabels(p.values.Role)
		clientIstioEgressNP.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: p.values.IstioIngressGateway.Labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: p.namespace}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						v1beta1constants.LabelApp:  etcdStatefulSetLabelAppValue,
						v1beta1constants.LabelRole: p.values.Role,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Port:     new(intstr.FromInt32(etcdconstants.PortEtcdClient)),
					Protocol: new(corev1.ProtocolTCP),
				}},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile Istio egress network policy for etcd client exposure: %w", err)
	}

	return nil
}

func (p *peerExposure) Destroy(ctx context.Context) error {
	objects := []client.Object{p.emptyGateway(), p.emptyVirtualService(), p.emptyNetworkPolicy(), p.emptyIstioEgressNetworkPolicy()}
	for i := range p.values.Members {
		objects = append(objects, p.emptyPodServiceEntry(i))
	}
	if p.values.ClientHost != "" {
		objects = append(objects, p.emptyClientGateway(), p.emptyClientVirtualService(), p.emptyClientServiceEntry(), p.emptyClientNetworkPolicy(), p.emptyClientIstioEgressNetworkPolicy())
	}
	return kubernetesutils.DeleteObjects(ctx, p.client, objects...)
}

func (p *peerExposure) Wait(_ context.Context) error        { return nil }
func (p *peerExposure) WaitCleanup(_ context.Context) error { return nil }

func (p *peerExposure) emptyGateway() *istionetworkingv1beta1.Gateway {
	return &istionetworkingv1beta1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: p.name(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyVirtualService() *istionetworkingv1beta1.VirtualService {
	return &istionetworkingv1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: p.name(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyPodServiceEntry(i int) *istionetworkingv1beta1.ServiceEntry {
	return &istionetworkingv1beta1.ServiceEntry{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", p.name(), i), Namespace: p.namespace}}
}

func (p *peerExposure) emptyNetworkPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-" + p.name(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyIstioEgressNetworkPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-" + p.name() + "-" + p.namespace, Namespace: p.values.IstioIngressGateway.Namespace}}
}

func (p *peerExposure) clientName() string {
	return fmt.Sprintf("etcd-%s-client", p.values.Role)
}

// clientServiceHost returns the in-cluster FQDN of the client Service for routing cross-seed client traffic.
func (p *peerExposure) clientServiceHost() string {
	return kubernetesutils.FQDNForService(fmt.Sprintf("etcd-%s-client", p.values.Role), p.namespace)
}

func (p *peerExposure) emptyClientGateway() *istionetworkingv1beta1.Gateway {
	return &istionetworkingv1beta1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: p.clientName(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyClientVirtualService() *istionetworkingv1beta1.VirtualService {
	return &istionetworkingv1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: p.clientName(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyClientServiceEntry() *istionetworkingv1beta1.ServiceEntry {
	return &istionetworkingv1beta1.ServiceEntry{ObjectMeta: metav1.ObjectMeta{Name: p.clientName(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyClientNetworkPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-" + p.clientName(), Namespace: p.namespace}}
}

func (p *peerExposure) emptyClientIstioEgressNetworkPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-" + p.clientName() + "-" + p.namespace, Namespace: p.values.IstioIngressGateway.Namespace}}
}

func getLabels(role string) map[string]string {
	return map[string]string{
		v1beta1constants.LabelApp:  "etcd-peer-exposure",
		v1beta1constants.LabelRole: role,
	}
}

// serviceEntryForExport creates a ServiceEntry with resolution: DNS that exports the named host to the given ingress
// namespace. Gardener seeds set defaultServiceExportTo: ["~"], so without this the Istio ingress proxy cannot discover
// the backend and returns NO_CLUSTER_FOUND. A DestinationRule exportTo only exports traffic policies, not endpoint
// visibility — ServiceEntry is the correct primitive for endpoint export.
func serviceEntryForExport(se *istionetworkingv1beta1.ServiceEntry, labels map[string]string, host, ingressNamespace string, port uint32, portName string) func() error {
	return func() error {
		se.Labels = labels
		se.Spec = istioapinetworkingv1beta1.ServiceEntry{
			Hosts:    []string{host},
			ExportTo: []string{ingressNamespace},
			Ports: []*istioapinetworkingv1beta1.ServicePort{{
				Number:   port,
				Name:     portName,
				Protocol: "TLS",
			}},
			Resolution: istioapinetworkingv1beta1.ServiceEntry_DNS,
		}
		return nil
	}
}

// gatewayWithPeerTLSPassthrough configures an Istio gateway with one Server entry per etcd member. Each Server
// listens on the member's unique external port (ExternalPort) with TLS passthrough so that etcd's URLStringsEqual,
// which falls back to DNS resolution, sees distinct IP:port tuples and cannot conflate members that share the same
// ingress IP. SNI is still used alongside the port in the VirtualService for correct cross-shoot routing.
func gatewayWithPeerTLSPassthrough(gateway *istionetworkingv1beta1.Gateway, labels, istioLabels map[string]string, members []PeerMember) func() error {
	return func() error {
		gateway.Labels = labels
		servers := make([]*istioapinetworkingv1beta1.Server, len(members))
		for i, m := range members {
			servers[i] = &istioapinetworkingv1beta1.Server{
				Hosts: []string{m.SNIHost},
				Port: &istioapinetworkingv1beta1.Port{
					Number:   m.ExternalPort,
					Name:     fmt.Sprintf("tls-etcd-peer-%d", i),
					Protocol: "TLS",
				},
				Tls: &istioapinetworkingv1beta1.ServerTLSSettings{
					Mode: istioapinetworkingv1beta1.ServerTLSSettings_PASSTHROUGH,
				},
			}
		}
		gateway.Spec = istioapinetworkingv1beta1.Gateway{
			Selector: istioLabels,
			Servers:  servers,
		}
		return nil
	}
}

// virtualServiceWithPeerSNIMatch configures an Istio virtual service with one TLS route per member. Each route
// matches on both the member's unique external port and its SNI host: the port ensures URLStringsEqual sees distinct
// IP:port tuples, while the SNI host ensures correct routing when multiple shoots share the same port index.
func virtualServiceWithPeerSNIMatch(virtualService *istionetworkingv1beta1.VirtualService, labels map[string]string, exportTo []string, members []PeerMember, gatewayName string) func() error {
	return func() error {
		virtualService.Labels = labels
		routes := make([]*istioapinetworkingv1beta1.TLSRoute, len(members))
		allHosts := make([]string, len(members))
		for i, m := range members {
			allHosts[i] = m.SNIHost
			routes[i] = &istioapinetworkingv1beta1.TLSRoute{
				Match: []*istioapinetworkingv1beta1.TLSMatchAttributes{{
					Port:     m.ExternalPort,
					SniHosts: []string{m.SNIHost},
				}},
				Route: []*istioapinetworkingv1beta1.RouteDestination{{
					Destination: &istioapinetworkingv1beta1.Destination{
						Host: m.PodFQDN,
						Port: &istioapinetworkingv1beta1.PortSelector{Number: uint32(etcdconstants.PortEtcdPeer)},
					},
				}},
			}
		}
		virtualService.Spec = istioapinetworkingv1beta1.VirtualService{
			ExportTo: exportTo,
			Hosts:    allHosts,
			Gateways: []string{gatewayName},
			Tls:      routes,
		}
		return nil
	}
}

// gatewayWithClientTLSPassthrough configures an Istio gateway that passes TLS through on the etcd client port so
// that cross-seed backup-restore containers can reach the source etcd for member-add operations.
func gatewayWithClientTLSPassthrough(gateway *istionetworkingv1beta1.Gateway, labels, istioLabels map[string]string, hosts []string) func() error {
	return func() error {
		gateway.Labels = labels
		gateway.Spec = istioapinetworkingv1beta1.Gateway{
			Selector: istioLabels,
			Servers: []*istioapinetworkingv1beta1.Server{{
				Hosts: hosts,
				Port: &istioapinetworkingv1beta1.Port{
					Number:   uint32(etcdconstants.PortEtcdClient),
					Name:     "tls-etcd-client",
					Protocol: "TLS",
				},
				Tls: &istioapinetworkingv1beta1.ServerTLSSettings{
					Mode: istioapinetworkingv1beta1.ServerTLSSettings_PASSTHROUGH,
				},
			}},
		}
		return nil
	}
}

// virtualServiceWithClientSNIMatch configures an Istio virtual service that routes the client SNI host to the etcd
// client Service on the etcd client port.
func virtualServiceWithClientSNIMatch(virtualService *istionetworkingv1beta1.VirtualService, labels map[string]string, exportTo, hosts []string, gatewayName, destinationHost string) func() error {
	return func() error {
		virtualService.Labels = labels
		virtualService.Spec = istioapinetworkingv1beta1.VirtualService{
			ExportTo: exportTo,
			Hosts:    hosts,
			Gateways: []string{gatewayName},
			Tls: []*istioapinetworkingv1beta1.TLSRoute{{
				Match: []*istioapinetworkingv1beta1.TLSMatchAttributes{{
					Port:     uint32(etcdconstants.PortEtcdClient),
					SniHosts: hosts,
				}},
				Route: []*istioapinetworkingv1beta1.RouteDestination{{
					Destination: &istioapinetworkingv1beta1.Destination{
						Host: destinationHost,
						Port: &istioapinetworkingv1beta1.PortSelector{Number: uint32(etcdconstants.PortEtcdClient)},
					},
				}},
			}},
		}
		return nil
	}
}
