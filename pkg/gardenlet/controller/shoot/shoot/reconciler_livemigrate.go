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
	botanistpkg "github.com/gardener/gardener/pkg/gardenlet/operation/botanist"
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

		// Build the operation and botanist lazily, only when this gardenlet actually has a step to execute.
		o, result, err := r.prepareOperation(ctx, log, shoot)
		if err != nil || o == nil {
			return result, err
		}
		botanist, err := botanistpkg.New(ctx, o)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to create botanist for live migration: %w", err)
		}

		log.Info("Executing live migration step", "step", step.conditionType)
		done, err := r.runLiveMigrationStep(ctx, log, botanist, step)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to execute live migration step %q: %w", step.conditionType, err)
		}

		if err := r.setLiveMigrationStepCondition(ctx, shoot, step, done); err != nil {
			return reconcile.Result{}, err
		}

		// Requeue so the next reconciliation re-evaluates the (possibly newly satisfied) condition and either proceeds
		// to the next step or hands off to the peer gardenlet.
		return reconcile.Result{RequeueAfter: liveMigrationRequeueAfter}, nil
	}

	log.Info("Live control plane migration completed")
	return reconcile.Result{}, nil
}

// setLiveMigrationStepCondition flips the step's tracking condition to True when the step reported completion, or keeps
// it Progressing otherwise.
func (r *Reconciler) setLiveMigrationStepCondition(ctx context.Context, shoot *gardencorev1beta1.Shoot, step liveMigrationStep, done bool) error {
	condition := v1beta1helper.GetOrInitConditionWithClock(r.Clock, v1beta1helper.GetLiveMigrationConditions(shoot), step.conditionType)
	if done {
		condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionTrue, "StepCompleted", "The live migration step has been completed.")
	} else {
		condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionProgressing, "StepInProgress", "The live migration step is in progress.")
	}
	return r.patchLiveMigrationConditions(ctx, shoot, condition)
}

// runLiveMigrationStep executes a single, this-gardenlet-owned live migration step. It returns whether the step has
// been completed (i.e. its tracking condition may be set to True). A step that is still in progress returns
// (false, nil) so the reconciler requeues and re-evaluates it.
//
// TODO(@acumino): The steps that deploy the destination control plane and perform the DNS/Istio cutover
//
//	(DestinationKubeAPIServerReady, MigrateDNSRecords) and the source cleanup (SourceSeedCleanup) reuse the large
//	botanist flows from runReconcileShootFlow/runMigrateShootFlow. They are wired incrementally; the etcd-formation
//	and endpoint-publishing steps are implemented below.
func (r *Reconciler) runLiveMigrationStep(ctx context.Context, log logr.Logger, botanist *botanistpkg.Botanist, step liveMigrationStep) (bool, error) {
	switch step.conditionType {
	case gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin:
		// The source persists its ShootState (including ca-etcd-peer) so the destination can restore it,
		// exposes its etcd peer ports across seeds, and (once the destination has exposed its peers)
		// redeploys etcd so it advertises the destination members as additional peers.
		if err := botanist.PersistShootState(ctx); err != nil {
			return false, fmt.Errorf("failed to persist shoot state: %w", err)

		}
		if err := botanist.DeployEtcdPeerExposure(ctx); err != nil {
			return false, err
		}
		if v1beta1helper.GetLiveMigrationCondition(botanist.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined) == nil {
			log.Info("Waiting for the destination gardenlet to expose its etcd peers before preparing the source etcd for peer join")
			return false, nil
		}
		if err := botanist.DeployEtcd(ctx); err != nil {
			return false, err
		}
		if err := botanist.WaitUntilEtcdsReady(ctx); err != nil {
			return false, err
		}
		return true, nil

	case gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined:
		// The destination restores secrets from ShootState (including ca-etcd-peer) so it shares the source CA,
		// exposes its etcd peer ports across seeds, and (once the source is ready) deploys etcd
		// configured to join the existing source cluster. It waits until all members (source + destination) are ready.
		if err := botanist.InitializeSecretsManagement(ctx); err != nil {
			return false, fmt.Errorf("failed to initialize secrets management: %w", err)

		}
		if err := botanist.DeployEtcdPeerExposure(ctx); err != nil {
			return false, err
		}
		if !v1beta1helper.IsLiveMigrationConditionTrue(botanist.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin) {
			log.Info("Waiting for the source gardenlet to prepare its etcd for peer join before joining the source cluster")
			return false, nil
		}
		if err := botanist.DeployEtcd(ctx); err != nil {
			return false, err
		}
		if err := botanist.WaitUntilEtcdsReady(ctx); err != nil {
			return false, err
		}
		return true, nil

	case gardencorev1beta1.ShootLiveMigrationSourceSeedCleanup:
		// Remove the cross-seed peer exposure of the source seed as part of the source cleanup.
		if err := botanist.DestroyEtcdPeerExposure(ctx); err != nil {
			return false, err
		}
		return true, nil

	default:
		// The remaining steps (extension migration, destination kube-apiserver, DNS cutover, etcd migration
		// verification, completion) are not yet wired. Keep them Progressing so the migration does not spuriously
		// advance past unimplemented work.
		log.Info("Live migration step is not yet implemented; keeping it in progress", "step", step.conditionType)
		return false, nil
	}
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
