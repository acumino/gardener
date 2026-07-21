// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package peerexposure_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	istioapinetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/gardener/pkg/component"
	. "github.com/gardener/gardener/pkg/component/etcd/peerexposure"
	. "github.com/gardener/gardener/pkg/utils/test/matchers"
)

var _ = Describe("PeerExposure", func() {
	var (
		ctx = context.Background()

		c          client.Client
		component_ component.DeployWaiter

		namespace   = "shoot--foo--bar"
		istioLabels = map[string]string{"istio": "ingressgateway"}
		members     = []PeerMember{
			{
				SNIHost:      "src-etcd-main-0.ingress.seed.example.com",
				PodFQDN:      "etcd-main-0.etcd-main-peer." + "shoot--foo--bar" + ".svc.cluster.local",
				ExternalPort: 2380,
			},
		}

		gateway        *istionetworkingv1beta1.Gateway
		virtualService *istionetworkingv1beta1.VirtualService
	)

	BeforeEach(func() {
		s := runtime.NewScheme()
		Expect(istionetworkingv1beta1.AddToScheme(s)).To(Succeed())
		Expect(networkingv1.AddToScheme(s)).To(Succeed())
		c = fakeclient.NewClientBuilder().WithScheme(s).Build()

		component_ = New(c, namespace, Values{
			Role:                "main",
			Members:             members,
			IstioIngressGateway: IstioIngressGateway{Namespace: "istio-ingress", Labels: istioLabels},
		})

		gateway = &istionetworkingv1beta1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "etcd-main-peer", Namespace: namespace}}
		virtualService = &istionetworkingv1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: "etcd-main-peer", Namespace: namespace}}
	})

	Describe("#Deploy", func() {
		It("should create the gateway with TLS passthrough on the etcd peer port", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(gateway), gateway)).To(Succeed())
			Expect(gateway.Spec.Selector).To(Equal(istioLabels))
			Expect(gateway.Spec.Servers).To(HaveLen(1))
			server := gateway.Spec.Servers[0]
			Expect(server.Hosts).To(Equal([]string{members[0].SNIHost}))
			Expect(server.Port.Number).To(Equal(uint32(2380)))
			Expect(server.Port.Protocol).To(Equal("TLS"))
			Expect(server.Tls.Mode).To(Equal(istioapinetworkingv1beta1.ServerTLSSettings_PASSTHROUGH))
		})

		It("should create the virtual service with one per-member route so each SNI host reaches its own pod", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(virtualService), virtualService)).To(Succeed())
			Expect(virtualService.Spec.Hosts).To(Equal([]string{members[0].SNIHost}))
			Expect(virtualService.Spec.Gateways).To(Equal([]string{"etcd-main-peer"}))
			Expect(virtualService.Spec.Tls).To(HaveLen(1))
			route := virtualService.Spec.Tls[0]
			Expect(route.Match[0].Port).To(Equal(uint32(2380)))
			Expect(route.Match[0].SniHosts).To(Equal([]string{members[0].SNIHost}))
			Expect(route.Route[0].Destination.Host).To(Equal(members[0].PodFQDN))
			Expect(route.Route[0].Destination.Port.Number).To(Equal(uint32(2380)))
		})

		It("should create a ServiceEntry per member exporting the pod subdomain to the ingress namespace", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())

			se := &istionetworkingv1beta1.ServiceEntry{ObjectMeta: metav1.ObjectMeta{Name: "etcd-main-peer-0", Namespace: namespace}}
			Expect(c.Get(ctx, client.ObjectKeyFromObject(se), se)).To(Succeed())
			Expect(se.Spec.Hosts).To(Equal([]string{members[0].PodFQDN}))
			Expect(se.Spec.ExportTo).To(Equal([]string{"istio-ingress"}))
			Expect(se.Spec.Resolution).To(Equal(istioapinetworkingv1beta1.ServiceEntry_DNS))
			Expect(se.Spec.Ports).To(HaveLen(1))
			Expect(se.Spec.Ports[0].Number).To(Equal(uint32(2380)))
		})

		It("should create a network policy allowing the istio ingress gateway to reach the etcd peer port", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())

			networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-etcd-main-peer", Namespace: namespace}}
			Expect(c.Get(ctx, client.ObjectKeyFromObject(networkPolicy), networkPolicy)).To(Succeed())
			Expect(networkPolicy.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", "etcd-statefulset"))
			Expect(networkPolicy.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("role", "main"))
			Expect(networkPolicy.Spec.Ingress).To(HaveLen(1))
			Expect(networkPolicy.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(Equal(istioLabels))
			Expect(networkPolicy.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(2380))
		})

		It("should create an egress network policy in the istio-ingress namespace allowing the gateway to reach etcd pods on the peer port", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())

			egressNP := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-etcd-main-peer-" + namespace, Namespace: "istio-ingress"}}
			Expect(c.Get(ctx, client.ObjectKeyFromObject(egressNP), egressNP)).To(Succeed())
			Expect(egressNP.Spec.PodSelector.MatchLabels).To(Equal(istioLabels))
			Expect(egressNP.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeEgress))
			Expect(egressNP.Spec.Egress).To(HaveLen(1))
			Expect(egressNP.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", namespace))
			Expect(egressNP.Spec.Egress[0].To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("app", "etcd-statefulset"))
			Expect(egressNP.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(2380))
		})
	})

	Describe("#Destroy", func() {
		It("should delete the gateway, virtual service, service entries and network policies", func() {
			Expect(component_.Deploy(ctx)).To(Succeed())
			Expect(component_.Destroy(ctx)).To(Succeed())

			networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-etcd-main-peer", Namespace: namespace}}
			egressNP := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-istio-ingress-to-etcd-main-peer-" + namespace, Namespace: "istio-ingress"}}
			se := &istionetworkingv1beta1.ServiceEntry{ObjectMeta: metav1.ObjectMeta{Name: "etcd-main-peer-0", Namespace: namespace}}
			Expect(c.Get(ctx, client.ObjectKeyFromObject(gateway), gateway)).To(BeNotFoundError())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(virtualService), virtualService)).To(BeNotFoundError())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(networkPolicy), networkPolicy)).To(BeNotFoundError())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(egressNP), egressNP)).To(BeNotFoundError())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(se), se)).To(BeNotFoundError())
		})
	})
})
