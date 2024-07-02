// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist

import (
	"context"

	"k8s.io/utils/ptr"

	"github.com/gardener/gardener/imagevector"
	vpnseedserver "github.com/gardener/gardener/pkg/component/networking/vpn/seedserver"
	imagevectorutils "github.com/gardener/gardener/pkg/utils/imagevector"
)

// DefaultVPNSeedServer returns a deployer for the vpn-seed-server.
func (b *Botanist) DefaultVPNSeedServer() (vpnseedserver.Interface, error) {
	imageAPIServerProxy, err := imagevector.ImageVector().FindImage(imagevector.ImageNameApiserverProxy, imagevectorutils.RuntimeVersion(b.SeedVersion()), imagevectorutils.TargetVersion(b.ShootVersion()))
	if err != nil {
		return nil, err
	}

	imageVPNSeedServer, err := imagevector.ImageVector().FindImage(imagevector.ImageNameVpnSeedServer, imagevectorutils.RuntimeVersion(b.SeedVersion()), imagevectorutils.TargetVersion(b.ShootVersion()))
	if err != nil {
		return nil, err
	}

	values := vpnseedserver.Values{
		RuntimeKubernetesVersion: b.Seed.KubernetesVersion,
		ImageAPIServerProxy:      imageAPIServerProxy.String(),
		ImageVPNSeedServer:       imageVPNSeedServer.String(),
		Network: vpnseedserver.NetworkValues{
			PodCIDR:     b.Shoot.Networks.Pods.String(),
			ServiceCIDR: b.Shoot.Networks.Services.String(),
			// NodeCIDR is set in DeployVPNServer to handle dynamice node network CIDRs
			IPFamilies: b.Shoot.GetInfo().Spec.Networking.IPFamilies,
		},
		Replicas:                             b.Shoot.GetReplicas(1),
		HighAvailabilityEnabled:              b.Shoot.VPNHighAvailabilityEnabled,
		HighAvailabilityNumberOfSeedServers:  b.Shoot.VPNHighAvailabilityNumberOfSeedServers,
		HighAvailabilityNumberOfShootClients: b.Shoot.VPNHighAvailabilityNumberOfShootClients,
	}

	if b.ShootUsesDNS() {
		values.KubeAPIServerHost = ptr.To(b.outOfClusterAPIServerFQDN())
	}

	if b.Shoot.VPNHighAvailabilityEnabled {
		values.Replicas = b.Shoot.GetReplicas(int32(b.Shoot.VPNHighAvailabilityNumberOfSeedServers))
	}

	return vpnseedserver.New(
		b.SeedClientSet.Client(),
		b.Shoot.SeedNamespace,
		b.SecretsManager,
		func() string { return b.IstioNamespace() },
		values,
	), nil
}

// DeployVPNServer deploys the vpn-seed-server.
func (b *Botanist) DeployVPNServer(ctx context.Context, suffix string) error {
	b.Shoot.Components.ControlPlane.VPNSeedServer.SetNodeNetworkCIDR(b.Shoot.GetInfo().Spec.Networking.Nodes)
	b.Shoot.Components.ControlPlane.VPNSeedServer.SetSeedNamespaceObjectUID(b.SeedNamespaceObject.UID)
	b.Shoot.Components.ControlPlane.VPNSeedServer.SetSuffix(suffix)

	return b.Shoot.Components.ControlPlane.VPNSeedServer.Deploy(ctx)
}
