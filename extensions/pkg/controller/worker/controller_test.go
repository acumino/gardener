// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var _ = Describe("Controller", func() {
	Describe("#machineDeploymentStatusChangedPredicate", func() {
		var (
			oldMachineDeployment *machinev1alpha1.MachineDeployment
			machineDeployment    *machinev1alpha1.MachineDeployment
			p                    predicate.Predicate
		)

		BeforeEach(func() {
			machineDeployment = &machinev1alpha1.MachineDeployment{
				Spec: machinev1alpha1.MachineDeploymentSpec{
					Strategy: machinev1alpha1.MachineDeploymentStrategy{
						Type: machinev1alpha1.InPlaceUpdateMachineDeploymentStrategyType,
						InPlaceUpdate: &machinev1alpha1.InPlaceUpdateMachineDeployment{
							OrchestrationType: machinev1alpha1.OrchestrationTypeManual,
						},
					},
				},
				Status: machinev1alpha1.MachineDeploymentStatus{
					Replicas:        2,
					UpdatedReplicas: 2,
				},
			}

			oldMachineDeployment = machineDeployment.DeepCopy()
			p = machineDeploymentStatusChangedPredicate()
		})

		Describe("#Create", func() {
			It("should return false when object is not machineDeployment", func() {
				Expect(p.Create(event.CreateEvent{Object: &corev1.Secret{}})).To(BeFalse())
			})

			It("should return false when machineDeployment strategy type is not InPlace", func() {
				machineDeployment.Spec.Strategy.Type = machinev1alpha1.RollingUpdateMachineDeploymentStrategyType

				Expect(p.Create(event.CreateEvent{Object: machineDeployment})).To(BeFalse())
			})

			It("should return false when machineDeployment orchestration type is not manual", func() {
				machineDeployment.Spec.Strategy.InPlaceUpdate.OrchestrationType = machinev1alpha1.OrchestrationTypeAuto

				Expect(p.Create(event.CreateEvent{Object: machineDeployment})).To(BeFalse())

			})
		})

		Describe("#Update", func() {
			It("should return false because new object is not machineDeployment", func() {
				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: &corev1.Secret{},
				})).To(BeFalse())
			})

			It("should return false because old object is not machineDeployment", func() {
				Expect(p.Update(event.UpdateEvent{
					ObjectOld: &corev1.Secret{},
					ObjectNew: machineDeployment,
				})).To(BeFalse())
			})

			It("should return false because the machineDeployment strategy type is not InPlace", func() {
				machineDeployment.Spec.Strategy.Type = machinev1alpha1.RollingUpdateMachineDeploymentStrategyType

				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: machineDeployment,
				})).To(BeFalse())
			})

			It("should return false because the machineDeployment orchestration type is not manual", func() {
				machineDeployment.Spec.Strategy.InPlaceUpdate.OrchestrationType = machinev1alpha1.OrchestrationTypeAuto

				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: machineDeployment,
				})).To(BeFalse())
			})

			It("should return false because the old updatedReplicas are equal to new updateReplicas", func() {
				machineDeployment.Status.UpdatedReplicas = 2
				oldMachineDeployment.Status.UpdatedReplicas = 2

				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: machineDeployment,
				})).To(BeFalse())
			})

			It("should return false because the new updatedReplicas are not equal to replicas", func() {
				oldMachineDeployment.Status.UpdatedReplicas = 2
				machineDeployment.Status.UpdatedReplicas = 3
				machineDeployment.Status.Replicas = 2

				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: machineDeployment,
				})).To(BeFalse())
			})

			It("should return true when the new replicas are equal to updatedReplicas and the updatedReplicas differ from old", func() {
				oldMachineDeployment.Status.UpdatedReplicas = 2
				machineDeployment.Status.UpdatedReplicas = 3
				machineDeployment.Status.Replicas = 3

				Expect(p.Update(event.UpdateEvent{
					ObjectOld: oldMachineDeployment,
					ObjectNew: machineDeployment,
				})).To(BeTrue())
			})

		})

		Describe("#Delete", func() {
			It("should return false", func() {
				Expect(p.Delete(event.DeleteEvent{})).To(BeFalse())
			})
		})

		Describe("#Generic", func() {
			It("should return false", func() {
				Expect(p.Generic(event.GenericEvent{})).To(BeFalse())
			})
		})
	})
})
