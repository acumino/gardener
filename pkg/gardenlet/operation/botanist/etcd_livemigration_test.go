// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist_test

import (
	druidcorev1alpha1 "github.com/gardener/etcd-druid/api/core/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/gardener/gardener/pkg/gardenlet/operation/botanist"
)

var _ = Describe("Etcd LiveMigration", func() {
	Describe("#LiveMigrationEtcdPeerHost", func() {
		It("should compose the cross-seed SNI host from seed name, etcd name, ordinal, shoot namespace and ingress domain", func() {
			Expect(LiveMigrationEtcdPeerHost("src-seed", "shoot--p1--foo", "ingress.seed.example.com", "main", 0)).To(Equal("src-seed-etcd-main-0-shoot--p1--foo.ingress.seed.example.com"))
			Expect(LiveMigrationEtcdPeerHost("src-seed", "shoot--p1--foo", "ingress.seed.example.com", "events", 2)).To(Equal("src-seed-etcd-events-2-shoot--p1--foo.ingress.seed.example.com"))
		})
	})

	Describe("#ComputeMemberPeerURLs", func() {
		It("should compute one entry per member with distinct peer port URLs", func() {
			Expect(ComputeMemberPeerURLs("src-seed", "shoot--p1--foo", "ingress.seed.example.com", "main", 3)).To(Equal([]druidcorev1alpha1.MemberPeerURLs{
				{MemberName: "src-seed-etcd-main-0", URLs: []string{"https://src-seed-etcd-main-0-shoot--p1--foo.ingress.seed.example.com:2380"}},
				{MemberName: "src-seed-etcd-main-1", URLs: []string{"https://src-seed-etcd-main-1-shoot--p1--foo.ingress.seed.example.com:2381"}},
				{MemberName: "src-seed-etcd-main-2", URLs: []string{"https://src-seed-etcd-main-2-shoot--p1--foo.ingress.seed.example.com:2382"}},
			}))
		})

		It("should return an empty slice for zero replicas", func() {
			Expect(ComputeMemberPeerURLs("src-seed", "shoot--p1--foo", "ingress.seed.example.com", "main", 0)).To(BeEmpty())
		})
	})
})
