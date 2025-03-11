// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"

	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/gardener/gardener/extensions/pkg/controller/worker"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
)

var _ = Describe("Mapper", func() {
	var (
		ctx = context.TODO()

		namespace = "some-namespace"

		worker            *extensionsv1alpha1.Worker
		machineDeployment *machinev1alpha1.MachineDeployment
	)

	BeforeEach(func() {
		worker = &extensionsv1alpha1.Worker{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker",
				Namespace: namespace,
			},
		}

		machineDeployment = &machinev1alpha1.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-deployment",
				Namespace: namespace,
				Labels: map[string]string{
					"worker.gardener.cloud/name": worker.Name,
				},
			},
			Spec: machinev1alpha1.MachineDeploymentSpec{
				Strategy: machinev1alpha1.MachineDeploymentStrategy{
					Type: machinev1alpha1.InPlaceUpdateMachineDeploymentStrategyType,
					InPlaceUpdate: &machinev1alpha1.InPlaceUpdateMachineDeployment{
						OrchestrationType: machinev1alpha1.OrchestrationTypeManual,
					},
				},
			},
		}
	})

	Describe("#MachineDeploymentToWorkerMapper", func() {
		var mapper handler.MapFunc

		BeforeEach(func() {
			mapper = MachineDeploymentToWorkerMapper()
		})

		It("should return nil when the object is not a MachineDeployment", func() {
			Expect(mapper(ctx, &corev1.Secret{})).To(BeNil())
		})

		It("should return nil when the machineDeployment strategy is not InPlaceUpdateMachineDeploymentStrategyType", func() {
			machineDeployment.Spec.Strategy.Type = machinev1alpha1.RollingUpdateMachineDeploymentStrategyType

			Expect(mapper(ctx, machineDeployment)).To(BeNil())
		})

		It("should return nil when the machineDeployment orchestration type is not OrchestrationTypeManual", func() {
			machineDeployment.Spec.Strategy.InPlaceUpdate.OrchestrationType = machinev1alpha1.OrchestrationTypeAuto

			Expect(mapper(ctx, machineDeployment)).To(BeNil())
		})

		It("should return nil when the machineDeployment does not have a worker label", func() {
			delete(machineDeployment.Labels, "worker.gardener.cloud/name")

			Expect(mapper(ctx, machineDeployment)).To(BeNil())
		})

		It("should map the machineDeployment to the worker", func() {
			Expect(mapper(ctx, machineDeployment)).To(ConsistOf(
				reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      worker.Name,
						Namespace: namespace,
					},
				}))
		})
	})
})
