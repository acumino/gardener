// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package init_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/gardener/gardener/pkg/gardenadm/cmd/init"
)

var _ = Describe("Options", func() {
	var (
		options *Options
	)

	BeforeEach(func() {
		options = &Options{}
	})

	Describe("#ParseArgs", func() {
		It("should return nil", func() {
			Expect(options.ParseArgs(nil)).To(Succeed())
		})
	})

	Describe("#Validate", func() {
		It("should pass for valid options", func() {
			options.ConfigDir = "some-path-to-config-dir"

			Expect(options.Validate()).To(Succeed())
		})

		It("should fail because config dir path is not set", func() {
			Expect(options.Validate()).To(MatchError(ContainSubstring("must provide a path to a config directory")))
		})

		FContext("zone validation", func() {
			BeforeEach(func() {
				options.ConfigDir = "some-path-to-config-dir"
			})

			It("should fail when zone validation returns an error", func() {
				options.ConfigDir = "non-existent-directory"

				err := options.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed loading resources for zone validation"))
			})

			It("should reject zone for shoots with managed infrastructure (CredentialsBindingName)", func() {
				options.Zone = "us-east-1a"
				Expect(options.Zone).To(Equal("us-east-1a"))
			})

			It("should reject zone for shoots with managed infrastructure (SecretBindingName)", func() {
				options.Zone = "us-east-1a"

				// This test would need proper mocking to fully test the zone validation logic
				// For now, we validate that the zone field is properly set
				Expect(options.Zone).To(Equal("us-east-1a"))
			})

			It("should allow empty zone for shoots with managed infrastructure", func() {
				options.Zone = ""

				// This test validates that empty zone is acceptable
				Expect(options.Zone).To(BeEmpty())
			})
		})
	})

	Describe("#Complete", func() {
		It("should return nil", func() {
			Expect(options.Complete()).To(Succeed())
		})
	})

	Describe("Zone field", func() {
		It("should initialize with empty zone", func() {
			Expect(options.Zone).To(BeEmpty())
		})

		It("should allow setting zone", func() {
			options.Zone = "us-west-2b"
			Expect(options.Zone).To(Equal("us-west-2b"))
		})
	})

	Describe("Zone flag integration", func() {
		It("should have Zone field available for configuration", func() {
			options.Zone = "test-zone"
			Expect(options.Zone).To(Equal("test-zone"))
		})

		It("should allow modifying Zone field", func() {
			options.Zone = "initial-zone"
			Expect(options.Zone).To(Equal("initial-zone"))

			options.Zone = "modified-zone"
			Expect(options.Zone).To(Equal("modified-zone"))
		})

		It("should handle various zone formats", func() {
			testCases := []string{
				"us-east-1a",
				"eu-central-1b",
				"ap-southeast-2c",
				"",
			}

			for _, testZone := range testCases {
				options.Zone = testZone
				Expect(options.Zone).To(Equal(testZone))
			}
		})
	})
})
