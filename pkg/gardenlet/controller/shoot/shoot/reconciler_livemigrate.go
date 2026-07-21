// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package shoot

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1helper "github.com/gardener/gardener/pkg/api/core/v1beta1/helper"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
)

// liveMigrationStep describes a single step of a live control plane migration. Each step is owned by exactly one of the
// two involved gardenlets (source or destination) and is tracked via a condition of the given type on
// status.liveMigration.conditions. The condition is the only shared state between the two gardenlets and therefore acts
// as the cross-seed handshake: the owning gardenlet drives the step to completion and flips the condition to True,
// while the other gardenlet waits (requeues) until it observes that condition.
type liveMigrationStep struct {
	// conditionType is the condition tracking this step's progress.
	conditionType gardencorev1beta1.ConditionType
	// owner is the migration role responsible for executing this step.
	owner v1beta1helper.LiveMigrationRole
}

// liveMigrationSteps is the ordered list of steps a live control plane migration progresses through. The order encodes
// the strict dependency chain: a step may only start once all preceding steps report their condition as True. The
// mapping of steps to owners and condition types follows GEP-39.
//
// TODO(@acumino): The step bodies (etcd peer join, extension migration, kube-apiserver deployment, DNS cutover, source
//
//	cleanup) are implemented in later phases. The formation of the 6-member etcd cluster (steps
//	SourceEtcdPreparedForPeerJoin and DestinationEtcdPeersJoined) additionally depends on the etcd-druid
//	`bootstrapWithExistingCluster` field, which is not yet available (see gardener/etcd-druid#1237).
var liveMigrationSteps = []liveMigrationStep{
	{gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin, v1beta1helper.LiveMigrationRoleSource},
	{gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined, v1beta1helper.LiveMigrationRoleDestination},
	{gardencorev1beta1.ShootLiveMigrationMigrateExtensionsNeededBeforeKubeAPIServer, v1beta1helper.LiveMigrationRoleSource},
	{gardencorev1beta1.ShootLiveMigrationDestinationKubeAPIServerReady, v1beta1helper.LiveMigrationRoleDestination},
	{gardencorev1beta1.ShootLiveMigrationMigrateDNSRecords, v1beta1helper.LiveMigrationRoleDestination},
	{gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete, v1beta1helper.LiveMigrationRoleDestination},
	{gardencorev1beta1.ShootLiveMigrationSourceSeedCleanup, v1beta1helper.LiveMigrationRoleSource},
	{gardencorev1beta1.ShootLiveMigrationMigrationCompleted, v1beta1helper.LiveMigrationRoleDestination},
}

// liveMigrationRequeueAfter is the delay used when a gardenlet is waiting for the peer gardenlet to complete a step it
// depends on.
const liveMigrationRequeueAfter = 10 * time.Second

// liveMigrateShoot drives the calling gardenlet's part of a live control plane migration. Both the source and the
// destination gardenlet execute this function for the same shoot; each only performs the steps it owns and otherwise
// waits for the peer gardenlet by requeuing. Progress is communicated exclusively through
// status.liveMigration.conditions.
func (r *Reconciler) liveMigrateShoot(ctx context.Context, log logr.Logger, shoot *gardencorev1beta1.Shoot, role v1beta1helper.LiveMigrationRole) (reconcile.Result, error) {
	log = log.WithValues("operation", "live-migrate", "liveMigrationRole", role)

	if err := r.ensureLiveMigrationConditionsInitialized(ctx, shoot); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to initialize live migration conditions: %w", err)
	}

	// Walk the ordered steps. For each step, ensure all preceding steps are done. If the current step is owned by this
	// gardenlet, execute it; otherwise requeue and wait for the owning (peer) gardenlet to complete it.
	for _, step := range liveMigrationSteps {
		if v1beta1helper.IsLiveMigrationConditionTrue(shoot, step.conditionType) {
			continue
		}

		if step.owner != role {
			log.Info("Waiting for peer gardenlet to complete live migration step", "step", step.conditionType, "owner", step.owner)
			return reconcile.Result{RequeueAfter: liveMigrationRequeueAfter}, nil
		}

		log.Info("Executing live migration step", "step", step.conditionType)
		if err := r.runLiveMigrationStep(ctx, log, shoot, step); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to execute live migration step %q: %w", step.conditionType, err)
		}

		// The step has been driven forward; requeue so the next reconciliation re-evaluates the (possibly newly
		// satisfied) condition and either proceeds to the next step or hands off to the peer gardenlet.
		return reconcile.Result{RequeueAfter: liveMigrationRequeueAfter}, nil
	}

	log.Info("Live control plane migration completed")
	return reconcile.Result{}, nil
}

// runLiveMigrationStep executes a single, this-gardenlet-owned live migration step and updates its tracking condition.
//
// TODO(@acumino): Wire the concrete step bodies. This currently only manages the condition scaffolding so that the
//
//	cross-seed handshake and step ordering can be exercised behind the alpha feature gate. See the phased plan:
//	  - SourceEtcdPreparedForPeerJoin / DestinationEtcdPeersJoined: 6-member etcd formation (Phase 1 peer exposure +
//	    Phase 2 AdditionalAdvertisePeerURLs/SkipClientSANVerification/bootstrapWithExistingCluster wiring).
//	  - MigrateExtensionsNeededBeforeKubeAPIServer: reuse the migrate-extension tasks from runMigrateShootFlow.
//	  - DestinationKubeAPIServerReady: deploy the destination control plane against the (already replicated) etcd.
//	  - MigrateDNSRecords: DNS/Istio cutover to the destination kube-apiserver (the near-zero-downtime hinge).
//	  - EtcdMigrationComplete: verify the destination etcd is authoritative and healthy.
//	  - SourceSeedCleanup: tear down the source control plane (reuse runMigrateShootFlow teardown tasks).
//	  - MigrationCompleted: set status.seedName = spec.seedName and clear the live migration state.
func (r *Reconciler) runLiveMigrationStep(ctx context.Context, _ logr.Logger, shoot *gardencorev1beta1.Shoot, step liveMigrationStep) error {
	// Step bodies are not yet implemented (blocked on later phases and the etcd-druid dependency). For now, do not flip
	// the condition to True so the migration does not spuriously progress. Persist the condition as-is to record that
	// the owning gardenlet has picked up the step.
	condition := v1beta1helper.GetOrInitConditionWithClock(r.Clock, v1beta1helper.GetLiveMigrationConditions(shoot), step.conditionType)
	condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionProgressing, "StepInProgress", "The live migration step is in progress.")
	return r.patchLiveMigrationConditions(ctx, shoot, condition)
}

// ensureLiveMigrationConditionsInitialized makes sure status.liveMigration exists and holds an initialized condition
// for every migration step. This gives operators immediate visibility into the full step list from the start.
func (r *Reconciler) ensureLiveMigrationConditionsInitialized(ctx context.Context, shoot *gardencorev1beta1.Shoot) error {
	var missing []gardencorev1beta1.Condition
	for _, step := range liveMigrationSteps {
		if v1beta1helper.GetLiveMigrationCondition(shoot, step.conditionType) == nil {
			missing = append(missing, v1beta1helper.InitConditionWithClock(r.Clock, step.conditionType))
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return r.patchLiveMigrationConditions(ctx, shoot, missing...)
}

// patchLiveMigrationConditions merges the given conditions into status.liveMigration.conditions and patches the shoot
// status. It only ever touches the conditions slice (via a strategic merge patch), so the source and destination
// gardenlets can update their respective conditions concurrently without clobbering one another.
func (r *Reconciler) patchLiveMigrationConditions(ctx context.Context, shoot *gardencorev1beta1.Shoot, conditions ...gardencorev1beta1.Condition) error {
	patch := client.StrategicMergeFrom(shoot.DeepCopy())

	if shoot.Status.LiveMigration == nil {
		shoot.Status.LiveMigration = &gardencorev1beta1.LiveMigration{}
	}
	shoot.Status.LiveMigration.Conditions = v1beta1helper.MergeConditions(shoot.Status.LiveMigration.Conditions, conditions...)

	if shoot.Status.MigrationStartTime == nil {
		now := metav1.NewTime(r.Clock.Now().UTC())
		shoot.Status.MigrationStartTime = &now
	}

	return r.GardenClient.Status().Patch(ctx, shoot, patch)
}
