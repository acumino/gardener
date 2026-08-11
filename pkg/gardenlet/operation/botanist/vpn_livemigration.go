// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/durationpb"
	istioapinetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	extensionsv1alpha1helper "github.com/gardener/gardener/pkg/api/extensions/v1alpha1/helper"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	extensionsdnsrecord "github.com/gardener/gardener/pkg/component/extensions/dnsrecord"
	vpnseedserver "github.com/gardener/gardener/pkg/component/networking/vpn/seedserver"
	vpnshoot "github.com/gardener/gardener/pkg/component/networking/vpn/shoot"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/imagevector"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	imagevectorutils "github.com/gardener/gardener/pkg/utils/imagevector"
	kubernetesutils "github.com/gardener/gardener/pkg/utils/kubernetes"
)

const dnsRecordLiveMigrationVPNName = "live-migration-vpn"

// LiveMigrationTemporaryVPNDNSName returns the hostname for the temporary VPN DNS record created during live
// control plane migration. It uses the "vpn-tmp" prefix on the shoot's internal cluster domain, mirroring the
// "api." prefix used by GetAPIServerDomain.
func LiveMigrationTemporaryVPNDNSName(internalClusterDomain string) string {
	return "vpn-tmp." + internalClusterDomain
}

// DefaultLiveMigrationVPNDNSRecord returns a DNSRecord deployer for vpn-tmp.<internalDomain>. It is built
// analogously to DefaultInternalDNSRecord but targets the temporary VPN subdomain.
func (b *Botanist) DefaultLiveMigrationVPNDNSRecord() extensionsdnsrecord.Interface {
	values := &extensionsdnsrecord.Values{
		Name:              b.Shoot.GetInfo().Name + "-" + dnsRecordLiveMigrationVPNName,
		SecretName:        DNSRecordSecretPrefix + "-" + b.Shoot.GetInfo().Name + "-" + dnsRecordLiveMigrationVPNName,
		Namespace:         b.Shoot.ControlPlaneNamespace,
		TTL:               b.dnsRecordTTLSeconds(),
		AnnotateOperation: true,
		IPStack:           gardenerutils.GetIPStackForShoot(b.Shoot.GetInfo()),
		Labels: map[string]string{
			v1beta1constants.LabelRole:  "live-migration-vpn",
			v1beta1constants.GardenRole: v1beta1constants.GardenRoleControlPlane,
		},
	}

	var credentialsDeployer extensionsdnsrecord.CredentialsDeployFunc
	if b.NeedsInternalDNS() {
		values.Type = b.Garden.InternalDomain.Provider
		if b.Garden.InternalDomain.Zone != "" {
			values.Zone = &b.Garden.InternalDomain.Zone
		}
		credentialsDeployer = extensionsdnsrecord.CredentialsDeployerFromCredentials(b.Garden.InternalDomain.Credentials, b.Shoot.GetInfo())
		values.DNSName = LiveMigrationTemporaryVPNDNSName(*b.Shoot.InternalClusterDomain)
	}

	return extensionsdnsrecord.New(
		b.Logger,
		b.SeedClientSet.Client(),
		values,
		extensionsdnsrecord.DefaultInterval,
		extensionsdnsrecord.DefaultSevereThreshold,
		extensionsdnsrecord.DefaultTimeout,
		credentialsDeployer,
	)
}

// DeployLiveMigrationVPNDNSRecord sets the record type and value from the destination seed's Istio LB address
// (b.APIServerAddress) and deploys the DNS record.
func (b *Botanist) DeployLiveMigrationVPNDNSRecord(ctx context.Context) error {
	b.Shoot.Components.Extensions.LiveMigrationVPNDNSRecord.SetRecordType(extensionsv1alpha1helper.GetDNSRecordType(b.APIServerAddress))
	b.Shoot.Components.Extensions.LiveMigrationVPNDNSRecord.SetValues([]string{b.APIServerAddress})
	return b.Shoot.Components.Extensions.LiveMigrationVPNDNSRecord.Deploy(ctx)
}

// DestroyLiveMigrationVPNDNSRecord destroys the vpn-tmp DNS record and waits for cleanup.
func (b *Botanist) DestroyLiveMigrationVPNDNSRecord(ctx context.Context) error {
	if err := b.Shoot.Components.Extensions.LiveMigrationVPNDNSRecord.Destroy(ctx); err != nil {
		return err
	}
	return b.Shoot.Components.Extensions.LiveMigrationVPNDNSRecord.WaitCleanup(ctx)
}

const vpnTmpIstioResourceName = "vpn-seed-server-tmp"

// DeployTemporaryVPNExposure creates the Istio Gateway, VirtualService and DestinationRule that expose the
// destination vpn-seed-server at vpn-tmp.<internalDomain>:8443 during live control plane migration. The Gateway
// uses TLS PASSTHROUGH so that OpenVPN's mTLS is preserved end-to-end.
func (b *Botanist) DeployTemporaryVPNExposure(ctx context.Context) error {
	host := LiveMigrationTemporaryVPNDNSName(*b.Shoot.InternalClusterDomain)
	istioNS := b.IstioNamespace()
	istioLabels := b.IstioLabels()

	// Gateway lives in the Istio namespace so the ingress gateway controller picks it up.
	gateway := &istionetworkingv1beta1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: istioNS},
	}
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, b.SeedClientSet.Client(), gateway, func() error {
		gateway.Spec = istioapinetworkingv1beta1.Gateway{
			Selector: istioLabels,
			Servers: []*istioapinetworkingv1beta1.Server{{
				Hosts: []string{host},
				Port: &istioapinetworkingv1beta1.Port{
					Number:   uint32(vpnseedserver.HTTPProxyGatewayPort),
					Name:     "tls-vpn-tmp",
					Protocol: "TLS",
				},
				Tls: &istioapinetworkingv1beta1.ServerTLSSettings{
					Mode: istioapinetworkingv1beta1.ServerTLSSettings_PASSTHROUGH,
				},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile Istio gateway for temporary VPN exposure: %w", err)
	}

	// VirtualService and DestinationRule live in the control-plane namespace so they can reference the
	// vpn-seed-server Service directly.
	virtualService := &istionetworkingv1beta1.VirtualService{
		ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: b.Shoot.ControlPlaneNamespace},
	}
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, b.SeedClientSet.Client(), virtualService, func() error {
		virtualService.Spec = istioapinetworkingv1beta1.VirtualService{
			ExportTo: []string{istioNS},
			Hosts:    []string{host},
			Gateways: []string{gateway.Name},
			Tls: []*istioapinetworkingv1beta1.TLSRoute{{
				Match: []*istioapinetworkingv1beta1.TLSMatchAttributes{{
					Port:     uint32(vpnseedserver.HTTPProxyGatewayPort),
					SniHosts: []string{host},
				}},
				Route: []*istioapinetworkingv1beta1.RouteDestination{{
					Destination: &istioapinetworkingv1beta1.Destination{
						Host: kubernetesutils.FQDNForService(vpnseedserver.ServiceName, b.Shoot.ControlPlaneNamespace),
						Port: &istioapinetworkingv1beta1.PortSelector{Number: uint32(vpnseedserver.EnvoyPort)},
					},
				}},
			}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile Istio virtual service for temporary VPN exposure: %w", err)
	}

	destinationRule := &istionetworkingv1beta1.DestinationRule{
		ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: b.Shoot.ControlPlaneNamespace},
	}
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, b.SeedClientSet.Client(), destinationRule, func() error {
		destinationRule.Spec = istioapinetworkingv1beta1.DestinationRule{
			ExportTo: []string{istioNS},
			Host:     kubernetesutils.FQDNForService(vpnseedserver.ServiceName, b.Shoot.ControlPlaneNamespace),
			TrafficPolicy: &istioapinetworkingv1beta1.TrafficPolicy{
				ConnectionPool: &istioapinetworkingv1beta1.ConnectionPoolSettings{
					Tcp: &istioapinetworkingv1beta1.ConnectionPoolSettings_TCPSettings{
						MaxConnections: 5000,
						TcpKeepalive: &istioapinetworkingv1beta1.ConnectionPoolSettings_TCPSettings_TcpKeepalive{
							Interval: &durationpb.Duration{Seconds: 75},
							Time:     &durationpb.Duration{Seconds: 7200},
						},
					},
				},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile Istio destination rule for temporary VPN exposure: %w", err)
	}

	return nil
}

// DestroyTemporaryVPNExposure removes the three Istio objects created by DeployTemporaryVPNExposure.
func (b *Botanist) DestroyTemporaryVPNExposure(ctx context.Context) error {
	return kubernetesutils.DeleteObjects(ctx, b.SeedClientSet.Client(),
		&istionetworkingv1beta1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: b.IstioNamespace()}},
		&istionetworkingv1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: b.Shoot.ControlPlaneNamespace}},
		&istionetworkingv1beta1.DestinationRule{ObjectMeta: metav1.ObjectMeta{Name: vpnTmpIstioResourceName, Namespace: b.Shoot.ControlPlaneNamespace}},
	)
}

// DefaultTemporaryVPNShoot returns a non-HA vpn-shoot-tmp component that tunnels to the destination seed via
// the dedicated vpn-tmp.<internalDomain> endpoint. It is deployed on the destination during live migration
// so extension admission webhooks in the shoot cluster are reachable from the destination kube-apiserver.
func (b *Botanist) DefaultTemporaryVPNShoot() (vpnshoot.Interface, error) {
	endpoint := LiveMigrationTemporaryVPNDNSName(*b.Shoot.InternalClusterDomain)

	image, err := imagevector.Containers().FindImage(imagevector.ContainerImageNameVpnClient, imagevectorutils.RuntimeVersion(b.ShootVersion()), imagevectorutils.TargetVersion(b.ShootVersion()))
	if err != nil {
		return nil, err
	}

	return vpnshoot.New(
		b.SeedClientSet.Client(),
		b.Shoot.ControlPlaneNamespace,
		b.SecretsManager,
		vpnshoot.Values{
			Image: image.String(),
			ReversedVPN: vpnshoot.ReversedVPNValues{
				Header:     "outbound|1194||" + vpnseedserver.ServiceName + "." + b.Shoot.ControlPlaneNamespace + ".svc.cluster.local",
				Endpoint:   endpoint + ".",
				IPFamilies: b.Shoot.GetInfo().Spec.Networking.IPFamilies,
			},
			HighAvailabilityEnabled:              false,
			HighAvailabilityNumberOfSeedServers:  1,
			HighAvailabilityNumberOfShootClients: 1,
			SeedPodNetwork:                       b.Seed.GetInfo().Spec.Networks.Pods,
			NameSuffix:                           "tmp",
		},
	), nil
}

// DeployTemporaryVPNShoot sets network CIDRs and deploys the vpn-shoot-tmp ManagedResource.
func (b *Botanist) DeployTemporaryVPNShoot(ctx context.Context) error {
	b.Shoot.Components.SystemComponents.TemporaryVPNShoot.SetPodNetworkCIDRs(b.Shoot.Networks.Pods)
	b.Shoot.Components.SystemComponents.TemporaryVPNShoot.SetServiceNetworkCIDRs(b.Shoot.Networks.Services)
	b.Shoot.Components.SystemComponents.TemporaryVPNShoot.SetNodeNetworkCIDRs(b.Shoot.Networks.Nodes)
	return b.Shoot.Components.SystemComponents.TemporaryVPNShoot.Deploy(ctx)
}

// DestroyTemporaryVPNShoot deletes the vpn-shoot-tmp ManagedResource.
func (b *Botanist) DestroyTemporaryVPNShoot(ctx context.Context) error {
	return b.Shoot.Components.SystemComponents.TemporaryVPNShoot.Destroy(ctx)
}
