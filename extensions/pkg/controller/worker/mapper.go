// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"

	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/controllerutils/mapper"
)

// ClusterToWorkerMapper returns a mapper that returns requests for Worker whose
// referenced clusters have been modified.
func ClusterToWorkerMapper(reader client.Reader, predicates []predicate.Predicate) handler.MapFunc {
	return mapper.ClusterToObjectMapper(reader, func() client.ObjectList { return &extensionsv1alpha1.WorkerList{} }, predicates)
}

// MachineDeploymentToWorkerMapper returns a mapper that returns requests for the Worker referenced by the machine deployment.
func MachineDeploymentToWorkerMapper() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		machineDeployment, ok := obj.(*machinev1alpha1.MachineDeployment)
		if !ok {
			return nil
		}

		if machineDeployment.Spec.Strategy.Type != machinev1alpha1.InPlaceUpdateMachineDeploymentStrategyType ||
			machineDeployment.Spec.Strategy.InPlaceUpdate == nil ||
			machineDeployment.Spec.Strategy.InPlaceUpdate.OrchestrationType != machinev1alpha1.OrchestrationTypeManual {
			return nil
		}

		workerName, ok := machineDeployment.Labels[v1beta1constants.LabelWorkerName]
		if !ok {
			return nil
		}

		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name:      workerName,
			Namespace: machineDeployment.Namespace,
		}}}
	}
}
