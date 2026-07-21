// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist

import (
	"context"
	"fmt"

	druidcorev1alpha1 "github.com/gardener/etcd-druid/api/core/v1alpha1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1helper "github.com/gardener/gardener/pkg/api/core/v1beta1/helper"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/component/etcd/etcd"
	etcdconstants "github.com/gardener/gardener/pkg/component/etcd/etcd/constants"
	"github.com/gardener/gardener/pkg/component/etcd/peerexposure"
)

// etcdLiveMigrationRoles are the etcd roles that participate in a live control plane migration.
// Only the main etcd is migrated live; the events etcd is re-created on the destination seed.
var etcdLiveMigrationRoles = []string{v1beta1constants.ETCDRoleMain}

// LiveMigrationEtcdPeerHost returns the SNI host under which the peer endpoint of the etcd member with the given role
// and ordinal on the given seed is reachable from other seeds. It is derived from the seed's ingress domain so that it
// resolves to the seed's shared Istio ingress gateway load balancer. The shoot namespace is appended after the ordinal
// so that concurrent migrations from the same seed produce distinct SNI hostnames without affecting the etcd member
// name (which matches the pod name and must not include the shoot namespace).
func LiveMigrationEtcdPeerHost(seedName, shootNamespace, ingressDomain, role string, ordinal int) string {
	return fmt.Sprintf("%s-%s.%s", liveMigrationEtcdMemberName(seedName, role, ordinal), shootNamespace, ingressDomain)
}

// LiveMigrationEtcdClientHost returns the SNI host under which the etcd client endpoint of the given seed is
// reachable from other seeds. It is used for cross-seed member-add operations during BootstrapWithExistingCluster.
func LiveMigrationEtcdClientHost(seedName, shootNamespace, ingressDomain, role string) string {
	return fmt.Sprintf("%s-%s-%s-client.%s", seedName, shootNamespace, etcd.Name(role), ingressDomain)
}

// liveMigrationEtcdMemberName returns the etcd member name for the given seed, role and ordinal. It matches
// etcd-druid's member naming (`<memberNamePrefix>-<etcd-name>-<ordinal>`), where the member name prefix is the seed
// name (see DefaultEtcd).
func liveMigrationEtcdMemberName(seedName, role string, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", seedName, etcd.Name(role), ordinal)
}

// ComputeMemberPeerURLs returns the cross-seed peer URLs for the etcd members of the given seed and role.
// Each member is reachable via its SNI hostname on the seed's Istio ingress gateway. The external port is
// PortEtcdPeer + ordinal so that URLStringsEqual (which resolves hostnames to IPs) sees a distinct IP:port
// per member and can correctly assign etcd member IDs during ValidateClusterAndAssignIDs.
func ComputeMemberPeerURLs(seedName, shootNamespace, ingressDomain, role string, replicas int32) []druidcorev1alpha1.MemberPeerURLs {
	memberPeerURLs := make([]druidcorev1alpha1.MemberPeerURLs, 0, replicas)
	for ordinal := 0; ordinal < int(replicas); ordinal++ {
		memberPeerURLs = append(memberPeerURLs, druidcorev1alpha1.MemberPeerURLs{
			MemberName: liveMigrationEtcdMemberName(seedName, role, ordinal),
			URLs:       []string{fmt.Sprintf("https://%s:%d", LiveMigrationEtcdPeerHost(seedName, shootNamespace, ingressDomain, role, ordinal), etcdconstants.PortEtcdPeer+int32(ordinal))},
		})
	}
	return memberPeerURLs
}

// DeployEtcdPeerExposure exposes the peer endpoints of this shoot's etcd members (main and events) across seeds via the
// seed's shared Istio ingress gateway. It is used during a live control plane migration (GEP-39) so that etcd members
// residing in different seeds can form a single cluster.
func (b *Botanist) DeployEtcdPeerExposure(ctx context.Context) error {
	seedName := b.Seed.GetInfo().Name
	shootNamespace := b.Shoot.ControlPlaneNamespace
	ingressDomain := b.Seed.IngressDomain()
	replicas := getEtcdReplicas(b.Shoot.GetInfo())

	for _, role := range etcdLiveMigrationRoles {
		members := make([]peerexposure.PeerMember, 0, replicas)
		for ordinal := 0; ordinal < int(replicas); ordinal++ {
			members = append(members, peerexposure.PeerMember{
				SNIHost:      LiveMigrationEtcdPeerHost(seedName, shootNamespace, ingressDomain, role, ordinal),
				PodFQDN:      etcdPodFQDN(role, ordinal, b.Shoot.ControlPlaneNamespace),
				ExternalPort: uint32(etcdconstants.PortEtcdPeer) + uint32(ordinal),
			})
		}

		component := peerexposure.New(b.SeedClientSet.Client(), b.Shoot.ControlPlaneNamespace, peerexposure.Values{
			Role:       role,
			Members:    members,
			ClientHost: LiveMigrationEtcdClientHost(seedName, shootNamespace, ingressDomain, role),
			IstioIngressGateway: peerexposure.IstioIngressGateway{
				Namespace: b.DefaultIstioNamespace(),
				Labels:    b.DefaultIstioLabels(),
			},
		})

		if err := component.Deploy(ctx); err != nil {
			return fmt.Errorf("failed to deploy etcd peer exposure for role %q: %w", role, err)
		}
	}

	return nil
}

// DestroyEtcdPeerExposure removes the cross-seed peer exposure of this shoot's etcd members.
func (b *Botanist) DestroyEtcdPeerExposure(ctx context.Context) error {
	seedName := b.Seed.GetInfo().Name
	shootNamespace := b.Shoot.ControlPlaneNamespace
	ingressDomain := b.Seed.IngressDomain()
	replicas := getEtcdReplicas(b.Shoot.GetInfo())

	for _, role := range etcdLiveMigrationRoles {
		members := make([]peerexposure.PeerMember, 0, replicas)
		for ordinal := 0; ordinal < int(replicas); ordinal++ {
			members = append(members, peerexposure.PeerMember{
				SNIHost:      LiveMigrationEtcdPeerHost(seedName, shootNamespace, ingressDomain, role, ordinal),
				PodFQDN:      etcdPodFQDN(role, ordinal, b.Shoot.ControlPlaneNamespace),
				ExternalPort: uint32(etcdconstants.PortEtcdPeer) + uint32(ordinal),
			})
		}

		component := peerexposure.New(b.SeedClientSet.Client(), b.Shoot.ControlPlaneNamespace, peerexposure.Values{
			Role:    role,
			Members: members,
			IstioIngressGateway: peerexposure.IstioIngressGateway{
				Namespace: b.DefaultIstioNamespace(),
				Labels:    b.DefaultIstioLabels(),
			},
		})

		if err := component.Destroy(ctx); err != nil {
			return fmt.Errorf("failed to destroy etcd peer exposure for role %q: %w", role, err)
		}
	}

	return nil
}

// etcdPodFQDN returns the in-cluster DNS subdomain for an etcd StatefulSet pod through the headless peer Service.
// The subdomain resolves to exactly one pod IP, giving Istio a stable per-member endpoint.
func etcdPodFQDN(role string, ordinal int, namespace string) string {
	// etcd-druid names pods <etcd-name>-<ordinal>; the headless peer service is <etcd-name>-peer.
	etcdName := etcd.Name(role)
	return fmt.Sprintf("%s-%d.%s-peer.%s.svc.cluster.local", etcdName, ordinal, etcdName, namespace)
}

// crossSeedPeerHostnames returns the combined Istio ingress peer hostnames for both the source and destination
// seeds (source first, then destination). The consistent ordering ensures the same config hash on both sides,
// so the secretsManager returns the already-restored source peer cert without regenerating it.
func crossSeedPeerHostnames(sourceSeedName, sourceIngressDomain, destSeedName, destIngressDomain, shootNamespace, role string, replicas int32) []string {
	hosts := make([]string, 0, 2*int(replicas))
	for ordinal := 0; ordinal < int(replicas); ordinal++ {
		hosts = append(hosts, LiveMigrationEtcdPeerHost(sourceSeedName, shootNamespace, sourceIngressDomain, role, ordinal))
	}
	for ordinal := 0; ordinal < int(replicas); ordinal++ {
		hosts = append(hosts, LiveMigrationEtcdPeerHost(destSeedName, shootNamespace, destIngressDomain, role, ordinal))
	}
	return hosts
}

// setLiveMigrationEtcdValues configures the cross-seed etcd values for a live control plane migration:
//   - The source sets AdditionalAdvertisePeerURLs to its OWN cross-seed peer URLs so other cluster members
//     can reach it via the Istio ingress gateway. Because the names use the source's own memberNamePrefix,
//     etcd-druid's CRD validation is satisfied.
//   - The destination sets BootstrapWithExistingCluster with the source's cross-seed peer URLs (the same
//     URLs the source advertised) so etcd-druid joins the existing source cluster.
//   - SkipClientSANVerification is set on both sides because cross-seed peer TLS certificates carry the
//     local service SAN, not the Istio-exposed hostname.
func (b *Botanist) setLiveMigrationEtcdValues(ctx context.Context, values *etcd.Values, role string) error {
	shoot := b.Shoot.GetInfo()
	if b.Shoot.IsSelfHosted() {
		return nil
	}

	liveMigrationRole := v1beta1helper.GetLiveMigrationRole(shoot, b.Seed.GetInfo().Name)
	if liveMigrationRole == v1beta1helper.LiveMigrationRoleNone {
		return nil
	}

	// Only etcd-main participates in the live migration joint cluster. etcd-events is re-created normally
	// on the destination and follows the standard deploy path.
	if role != v1beta1constants.ETCDRoleMain {
		return nil
	}

	values.SkipClientSANVerification = true

	replicas := getEtcdReplicas(shoot)
	shootNamespace := b.Shoot.ControlPlaneNamespace

	switch liveMigrationRole {
	case v1beta1helper.LiveMigrationRoleSource:
		// The source advertises its OWN members' cross-seed peer URLs so destination members can reach them.
		// etcd-druid validates that AdditionalAdvertisePeerURLs names follow the local memberNamePrefix pattern,
		// which is satisfied here because we use the local (source) seed's own member names.
		localSeedName := b.Seed.GetInfo().Name
		localIngressDomain := b.Seed.IngressDomain()
		values.AdditionalAdvertisePeerURLs = ComputeMemberPeerURLs(localSeedName, shootNamespace, localIngressDomain, role, replicas)
		// The source adds its Istio ingress client hostname to the etcd server TLS cert's SANs so that the
		// destination backup-restore can verify the server cert when connecting via --service-endpoints.
		values.ExtraClientServiceDNSNames = []string{LiveMigrationEtcdClientHost(localSeedName, shootNamespace, localIngressDomain, role)}

		// The source adds both its own and the destination's Istio peer hostnames to the peer TLS cert so that
		// cross-seed raft connections can be verified. The destination will be configured with the same
		// ExtraPeerServiceDNSNames, producing the same config hash, so secretsManager returns the already-restored
		// source cert without regenerating it.
		destSeedName := ptr.Deref(shoot.Spec.SeedName, "")
		destSeed := &gardencorev1beta1.Seed{}
		if err := b.GardenAPIReader.Get(ctx, client.ObjectKey{Name: destSeedName}, destSeed); err != nil {
			return fmt.Errorf("failed to get destination seed %q: %w", destSeedName, err)
		}
		destIngressDomain := ""
		if destSeed.Spec.Ingress != nil {
			destIngressDomain = destSeed.Spec.Ingress.Domain
		}
		values.ExtraPeerServiceDNSNames = crossSeedPeerHostnames(localSeedName, localIngressDomain, destSeedName, destIngressDomain, shootNamespace, role, replicas)

	case v1beta1helper.LiveMigrationRoleDestination:
		// The destination bootstraps from the source cluster using the source's cross-seed peer URLs.
		sourceSeedName := ptr.Deref(shoot.Status.SeedName, "")
		sourceSeed := &gardencorev1beta1.Seed{}
		if err := b.GardenAPIReader.Get(ctx, client.ObjectKey{Name: sourceSeedName}, sourceSeed); err != nil {
			return fmt.Errorf("failed to get source seed %q: %w", sourceSeedName, err)
		}
		sourceIngressDomain := ""
		if sourceSeed.Spec.Ingress != nil {
			sourceIngressDomain = sourceSeed.Spec.Ingress.Domain
		}

		sourcePeerURLs := ComputeMemberPeerURLs(sourceSeedName, shootNamespace, sourceIngressDomain, role, replicas)
		members := make([]druidcorev1alpha1.BootstrapExistingMember, 0, len(sourcePeerURLs))
		for _, m := range sourcePeerURLs {
			members = append(members, druidcorev1alpha1.BootstrapExistingMember{Name: m.MemberName, PeerURLs: m.URLs})
		}
		values.BootstrapWithExistingCluster = &druidcorev1alpha1.BootstrapWithExistingCluster{
			Members: members,
			ClientEndpoints: []string{
				fmt.Sprintf("https://%s:%d",
					LiveMigrationEtcdClientHost(sourceSeedName, shootNamespace, sourceIngressDomain, role),
					etcdconstants.PortEtcdClient),
			},
		}

		// Use the same ExtraPeerServiceDNSNames as the source so that the secretsManager produces the same config
		// hash and returns the source peer cert already restored from ShootState, which carries both sets of SANs.
		localSeedName := b.Seed.GetInfo().Name
		localIngressDomain := b.Seed.IngressDomain()
		values.ExtraPeerServiceDNSNames = crossSeedPeerHostnames(sourceSeedName, sourceIngressDomain, localSeedName, localIngressDomain, shootNamespace, role, replicas)
		// The destination also needs to advertise its own Istio peer hostnames so the source members can
		// reach it via the shared ingress gateway. Without this, initial-advertise-peer-urls and the
		// destination's initial-cluster entries only contain in-cluster DNS names the source cannot resolve.
		values.AdditionalAdvertisePeerURLs = ComputeMemberPeerURLs(localSeedName, shootNamespace, localIngressDomain, role, replicas)
	}

	return nil
}
