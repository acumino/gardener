// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package shoot

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1helper "github.com/gardener/gardener/pkg/api/core/v1beta1/helper"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/gardenlet/operation"
	botanistpkg "github.com/gardener/gardener/pkg/gardenlet/operation/botanist"
	"github.com/gardener/gardener/pkg/gardenlet/operation/shoot"
	errorsutils "github.com/gardener/gardener/pkg/utils/errors"
	"github.com/gardener/gardener/pkg/utils/flow"
	retryutils "github.com/gardener/gardener/pkg/utils/retry"
)

const (
	liveMigrationStepInterval = 10 * time.Second
	liveMigrationStepTimeout  = 10 * time.Minute
)

// liveMigrationStepOwners maps each live control plane migration condition to the gardenlet role responsible for
// executing that step. The flow graph looks up each step's owner here to decide to perform the step or wait for the peer gardenlet.
var liveMigrationStepOwners = map[gardencorev1beta1.ConditionType]v1beta1helper.LiveMigrationRole{
	gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin:              v1beta1helper.LiveMigrationRoleSource,
	gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined:                 v1beta1helper.LiveMigrationRoleDestination,
	gardencorev1beta1.ShootLiveMigrationMigrateExtensionsNeededBeforeKubeAPIServer: v1beta1helper.LiveMigrationRoleSource,
	gardencorev1beta1.ShootLiveMigrationDestinationKubeAPIServerReady:              v1beta1helper.LiveMigrationRoleDestination,
	gardencorev1beta1.ShootLiveMigrationMigrateSourceSeedComponents:                v1beta1helper.LiveMigrationRoleSource,
	gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete:                      v1beta1helper.LiveMigrationRoleDestination,
	gardencorev1beta1.ShootLiveMigrationMigrationCompleted:                         v1beta1helper.LiveMigrationRoleDestination,
	gardencorev1beta1.ShootLiveMigrationSourceSeedFinalTeardown:                    v1beta1helper.LiveMigrationRoleSource,

	gardencorev1beta1.ShootLiveMigrationAbortDestinationCleanup: v1beta1helper.LiveMigrationRoleDestination,
	gardencorev1beta1.ShootLiveMigrationAbortSourceRevert:       v1beta1helper.LiveMigrationRoleSource,
}

// liveMigrationConditionOrder is the canonical ordering of live migration conditions, matching the flow step sequence.
// Conditions are sorted by this order when written to status so they always appear in flow order regardless of which
// gardenlet set them first.
var liveMigrationConditionOrder = []gardencorev1beta1.ConditionType{
	gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin,
	gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined,
	gardencorev1beta1.ShootLiveMigrationMigrateExtensionsNeededBeforeKubeAPIServer,
	gardencorev1beta1.ShootLiveMigrationDestinationKubeAPIServerReady,
	gardencorev1beta1.ShootLiveMigrationMigrateSourceSeedComponents,
	gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete,
	gardencorev1beta1.ShootLiveMigrationMigrationCompleted,
	gardencorev1beta1.ShootLiveMigrationSourceSeedFinalTeardown,
}

func (r *Reconciler) runLiveMigrateShootFlow(ctx context.Context, o *operation.Operation, role v1beta1helper.LiveMigrationRole) *v1beta1helper.WrappedLastErrors {
	var (
		botanist        *botanistpkg.Botanist
		err             error
		tasksWithErrors []string
	)

	for _, lastError := range o.Shoot.GetInfo().Status.LastErrors {
		if lastError.TaskID != nil {
			tasksWithErrors = append(tasksWithErrors, *lastError.TaskID)
		}
	}

	errorContext := errorsutils.NewErrorContext("Shoot control plane live migration", tasksWithErrors)

	if err = errorsutils.HandleErrors(errorContext,
		func(errorID string) error {
			o.CleanShootTaskError(ctx, errorID)
			return nil
		},
		nil,
		errorsutils.ToExecute("Create botanist", func() error {
			return retryutils.UntilTimeout(ctx, liveMigrationStepInterval, liveMigrationStepTimeout, func(context.Context) (done bool, err error) {
				botanist, err = botanistpkg.New(ctx, o)
				if err != nil {
					return retryutils.MinorError(err)
				}
				return retryutils.Ok()
			})
		}),
	); err != nil {
		return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), err)
	}

	// Set before the graph runs so IsRestorePhase() is consistent across every task.
	o.Shoot.LiveMigrationRole = role

	// The live migration flow deploys the shoot control plane on the destination seed without running the
	// infrastructure reconciliation that normally populates o.Shoot.Networks (see WaitForInfrastructure). Compute
	// the networks here from the shoot's spec and status so control plane components (e.g. kube-apiserver, VPN) can
	// read the CIDRs. This mirrors the regular reconcile and migrate flows.
	if o.Shoot.GetInfo().Spec.Networking != nil && o.Shoot.GetInfo().Spec.Networking.Nodes != nil {
		networks, err := shoot.ToNetworks(o.Shoot.GetInfo(), o.Shoot.IsWorkerless)
		if err != nil {
			return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), err)
		}
		o.Shoot.Networks = networks
	}

	var (
		g = flow.NewGraph("Shoot control plane live migration")

		sourceEtcdReadyForPeerJoin = g.Add(flow.Task{
			Name: "Making source etcd ready for peer join",
			Fn:   r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationSourceEtcdPreparedForPeerJoin, sourceEtcdPreparedForPeerJoinTaskGroup(botanist)),
		})

		destinationEtcdPeersJoined = g.Add(flow.Task{
			Name:         "Joining destination etcd to the source cluster",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined, destinationEtcdPeersJoinedTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(sourceEtcdReadyForPeerJoin),
		})

		migrateExtensionsNeededBeforeKubeAPIServer = g.Add(flow.Task{
			Name:         "Migrating source extensions needed before kube-apiserver",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationMigrateExtensionsNeededBeforeKubeAPIServer, botanist.MigrateExtensionsBeforeKubeAPIServerTaskGroup(false)),
			Dependencies: flow.NewTaskIDs(destinationEtcdPeersJoined),
		})

		destinationKubeAPIServerReady = g.Add(flow.Task{
			Name:         "Deploying destination control plane and temporary VPN",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationDestinationKubeAPIServerReady, destinationKubeAPIServerReadyTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(migrateExtensionsNeededBeforeKubeAPIServer),
		})

		sourceSeedComponentsMigrated = g.Add(flow.Task{
			Name:         "Migrating components in the source seed",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationMigrateSourceSeedComponents, migrateSourceSeedComponentsTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(destinationKubeAPIServerReady),
		})

		etcdMigrationComplete = g.Add(flow.Task{
			Name:         "Migrating etcd cluster to destination seed",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete, etcdMigrationCompleteTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(sourceSeedComponentsMigrated),
		})

		migrationCompleted = g.Add(flow.Task{
			Name:         "Completing live migration",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationMigrationCompleted, destinationCompletionTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(etcdMigrationComplete),
		})

		_ = g.Add(flow.Task{
			Name:         "Tearing down components and namespace in source seed",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationSourceSeedFinalTeardown, sourceSeedFinalTeardownTaskGroup(botanist, o)),
			Dependencies: flow.NewTaskIDs(migrationCompleted),
		})
	)

	f := g.Compile()
	if err := f.Run(ctx, flow.Opts{
		Log:              botanist.Logger,
		ProgressReporter: r.newProgressReporter(botanist.ReportShootProgress),
		ErrorContext:     errorContext,
		ErrorCleaner:     botanist.CleanShootTaskError,
	}); err != nil {
		return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), flow.Errors(err))
	}

	return nil
}

// prepareLiveMigrationFlow constructs the botanist and error context shared by the live migration forward and abort
// flows, sets the live migration role (so IsRestorePhase() is consistent across every task) and computes the shoot
// networks (the live migration flow does not run the infrastructure reconciliation that normally populates them).
func (r *Reconciler) prepareLiveMigrationFlow(ctx context.Context, o *operation.Operation, role v1beta1helper.LiveMigrationRole) (*botanistpkg.Botanist, *errorsutils.ErrorContext, *v1beta1helper.WrappedLastErrors) {
	var (
		botanist        *botanistpkg.Botanist
		err             error
		tasksWithErrors []string
	)

	for _, lastError := range o.Shoot.GetInfo().Status.LastErrors {
		if lastError.TaskID != nil {
			tasksWithErrors = append(tasksWithErrors, *lastError.TaskID)
		}
	}

	errorContext := errorsutils.NewErrorContext("Shoot control plane live migration", tasksWithErrors)

	if err = errorsutils.HandleErrors(errorContext,
		func(errorID string) error {
			o.CleanShootTaskError(ctx, errorID)
			return nil
		},
		nil,
		errorsutils.ToExecute("Create botanist", func() error {
			return retryutils.UntilTimeout(ctx, liveMigrationStepInterval, liveMigrationStepTimeout, func(context.Context) (done bool, err error) {
				botanist, err = botanistpkg.New(ctx, o)
				if err != nil {
					return retryutils.MinorError(err)
				}
				return retryutils.Ok()
			})
		}),
	); err != nil {
		return nil, nil, v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), err)
	}

	// Set before the graph runs so IsRestorePhase() is consistent across every task.
	o.Shoot.LiveMigrationRole = role

	// The live migration flow deploys the shoot control plane on the destination seed without running the
	// infrastructure reconciliation that normally populates o.Shoot.Networks (see WaitForInfrastructure). Compute
	// the networks here from the shoot's spec and status so control plane components (e.g. kube-apiserver, VPN) can
	// read the CIDRs. This mirrors the regular reconcile and migrate flows.
	if o.Shoot.GetInfo().Spec.Networking != nil && o.Shoot.GetInfo().Spec.Networking.Nodes != nil {
		networks, err := shoot.ToNetworks(o.Shoot.GetInfo(), o.Shoot.IsWorkerless)
		if err != nil {
			return nil, nil, v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), err)
		}
		o.Shoot.Networks = networks
	}

	return botanist, errorContext, nil
}

// runAbortLiveMigrateShootFlow runs the abort flow for an in-progress live control plane migration. The destination
// gardenlet removes the etcd members it added and tears down its migration-specific resources; afterwards the source
// gardenlet reverts its etcd to a standalone cluster and cleans up its migration-specific resources. The two steps are
// coordinated via the ShootLiveMigrationAbort* conditions: the source revert only runs after the destination cleanup is
// complete, so the destination can still reach the source to deregister its members before the source peer URLs go away.
func (r *Reconciler) runAbortLiveMigrateShootFlow(ctx context.Context, o *operation.Operation, role v1beta1helper.LiveMigrationRole) *v1beta1helper.WrappedLastErrors {
	botanist, errorContext, werr := r.prepareLiveMigrationFlow(ctx, o, role)
	if werr != nil {
		return werr
	}

	var (
		g = flow.NewGraph("Shoot control plane live migration abort")

		destinationCleanup = g.Add(flow.Task{
			Name: "Cleaning up destination seed after abort",
			Fn:   r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationAbortDestinationCleanup, abortDestinationCleanupTaskGroup(ctx, botanist, o)),
		})
		_ = g.Add(flow.Task{
			Name:         "Reverting source seed etcd after abort",
			Fn:           r.executeStepOrWait(botanist, role, gardencorev1beta1.ShootLiveMigrationAbortSourceRevert, abortSourceRevertTaskGroup(botanist)),
			Dependencies: flow.NewTaskIDs(destinationCleanup),
		})
	)

	f := g.Compile()
	if err := f.Run(ctx, flow.Opts{
		Log:              botanist.Logger,
		ProgressReporter: r.newProgressReporter(botanist.ReportShootProgress),
		ErrorContext:     errorContext,
		ErrorCleaner:     botanist.CleanShootTaskError,
	}); err != nil {
		return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), flow.Errors(err))
	}

	return nil
}

func (r *Reconciler) executeStepOrWait(botanist *botanistpkg.Botanist, role v1beta1helper.LiveMigrationRole, conditionType gardencorev1beta1.ConditionType, group flow.TaskGroup) flow.TaskFn {
	owner := liveMigrationStepOwners[conditionType]
	return flow.TaskFn(func(ctx context.Context) error {
		if err := r.checkGardenletVersionParity(ctx, botanist.Shoot.GetInfo(), role); err != nil {
			return err
		}
		if role != owner {
			return r.waitForLiveMigrationPeerStep(ctx, botanist, conditionType)
		}
		if err := r.setLiveMigrationStepCondition(ctx, botanist.Shoot.GetInfo(), conditionType, false); err != nil {
			return err
		}

		g := flow.NewGraph(string(conditionType))
		g.AddGroup(group)
		if err := g.Compile().Run(ctx, flow.Opts{Log: botanist.Logger}); err != nil {
			taskErr := flow.Errors(err)
			if condErr := r.setLiveMigrationStepConditionError(ctx, botanist.Shoot.GetInfo(), conditionType, taskErr); condErr != nil {
				botanist.Logger.Error(condErr, "Failed to set error condition for live migration step", "step", conditionType)
			}
			return taskErr
		}
		return r.setLiveMigrationStepCondition(ctx, botanist.Shoot.GetInfo(), conditionType, true)
	}).RetryUntilTimeout(liveMigrationStepInterval, liveMigrationStepTimeout)
}

func (r *Reconciler) waitForLiveMigrationPeerStep(ctx context.Context, botanist *botanistpkg.Botanist, conditionType gardencorev1beta1.ConditionType) error {
	shoot := &gardencorev1beta1.Shoot{}
	if err := r.GardenClient.Get(ctx, client.ObjectKeyFromObject(botanist.Shoot.GetInfo()), shoot); err != nil {
		return fmt.Errorf("failed to read shoot while waiting for peer gardenlet to complete live migration step %q: %w", conditionType, err)
	}

	if !v1beta1helper.IsLiveMigrationConditionTrue(shoot, conditionType) {
		botanist.Logger.Info("Waiting for peer gardenlet to complete live migration step", "step", conditionType)
		return fmt.Errorf("waiting for peer gardenlet to complete live migration step %q", conditionType)
	}

	// Refresh the cached shoot so the next patchLiveMigrationConditions call has all conditions
	// (including those set by the peer gardenlet) in the local merge, preserving sort order.
	botanist.Shoot.SetInfo(shoot)
	return nil
}

func (r *Reconciler) setLiveMigrationStepCondition(ctx context.Context, shoot *gardencorev1beta1.Shoot, conditionType gardencorev1beta1.ConditionType, done bool) error {
	condition := v1beta1helper.GetOrInitConditionWithClock(r.Clock, v1beta1helper.GetLiveMigrationConditions(shoot), conditionType)
	if done {
		condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionTrue, "StepCompleted", "The live migration step has been completed.")
	} else {
		condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionProgressing, "StepInProgress", "The live migration step is in progress.")
	}
	return r.patchLiveMigrationConditions(ctx, shoot, condition)
}

func (r *Reconciler) setLiveMigrationStepConditionError(ctx context.Context, shoot *gardencorev1beta1.Shoot, conditionType gardencorev1beta1.ConditionType, err error) error {
	condition := v1beta1helper.GetOrInitConditionWithClock(r.Clock, v1beta1helper.GetLiveMigrationConditions(shoot), conditionType)
	condition = v1beta1helper.UpdatedConditionWithClock(r.Clock, condition, gardencorev1beta1.ConditionFalse, "StepFailed", err.Error())
	return r.patchLiveMigrationConditions(ctx, shoot, condition)
}

// finalizeLiveMigration advances status.seedName so the destination gardenlet takes ownership,
// removes all queued task annotations, and removes the live-migration intent annotation.
func (r *Reconciler) finalizeLiveMigration(ctx context.Context, shoot *gardencorev1beta1.Shoot) error {
	statusPatch := client.StrategicMergeFrom(shoot.DeepCopy())
	shoot.Status.SeedName = shoot.Spec.SeedName
	if err := r.GardenClient.Status().Patch(ctx, shoot, statusPatch); err != nil {
		return fmt.Errorf("failed to update live migration seed name: %w", err)
	}

	patch := client.MergeFrom(shoot.DeepCopy())
	controllerutils.RemoveAllTasks(shoot.Annotations)
	delete(shoot.Annotations, v1beta1constants.AnnotationMigrationLiveMigrate)
	return r.GardenClient.Patch(ctx, shoot, patch)
}

// finalizeAbortLiveMigration reverts spec.seedName back to the source seed (status.seedName) and clears the
// live-migration state and the live-migrate and abort-live-migrate annotations, returning the shoot to normal
// reconciliation on the source seed. It runs on the source gardenlet after both abort cleanup steps have completed.
func (r *Reconciler) finalizeAbortLiveMigration(ctx context.Context, shoot *gardencorev1beta1.Shoot) error {
	patch := client.MergeFrom(shoot.DeepCopy())
	// Revert spec.seedName to the source seed so the shoot is no longer considered to be migrating. The gardener-apiserver
	// admission allows this direct spec.seedName revert while the abort annotation is present (see the shoot validator).
	shoot.Spec.SeedName = shoot.Status.SeedName
	delete(shoot.Annotations, v1beta1constants.AnnotationMigrationLiveMigrate)
	delete(shoot.Annotations, v1beta1constants.AnnotationMigrationAbortLiveMigrate)
	if err := r.GardenClient.Patch(ctx, shoot, patch); err != nil {
		return fmt.Errorf("failed to revert spec.seedName and remove live migration annotations: %w", err)
	}

	statusPatch := client.StrategicMergeFrom(shoot.DeepCopy())
	shoot.Status.LiveMigration = nil
	shoot.Status.MigrationStartTime = nil
	return r.GardenClient.Status().Patch(ctx, shoot, statusPatch)
}

func (r *Reconciler) patchLiveMigrationConditions(ctx context.Context, shoot *gardencorev1beta1.Shoot, conditions ...gardencorev1beta1.Condition) error {
	// Use an empty conditions list as the strategic-merge-patch base so that ALL merged conditions appear in the patch payload in sorted order.
	// A minimal diff (only the changed item) causes the server to re-order the list: the patched item floats to the front while unchanged conditions are appended as "server-only" items,
	// breaking the canonical flow order. Sending the full sorted list makes the server apply them in patch order, which matches liveMigrationConditionOrder.
	base := shoot.DeepCopy()
	base.Status.LiveMigration = &gardencorev1beta1.LiveMigration{}
	patch := client.StrategicMergeFrom(base)

	if shoot.Status.LiveMigration == nil {
		shoot.Status.LiveMigration = &gardencorev1beta1.LiveMigration{}
	}
	merged := v1beta1helper.MergeConditions(shoot.Status.LiveMigration.Conditions, conditions...)
	slices.SortStableFunc(merged, func(a, b gardencorev1beta1.Condition) int {
		return cmp.Compare(slices.Index(liveMigrationConditionOrder, a.Type), slices.Index(liveMigrationConditionOrder, b.Type))
	})
	shoot.Status.LiveMigration.Conditions = merged

	return r.GardenClient.Status().Patch(ctx, shoot, patch)
}

// taskGroupSourceEtcdPreparedForPeerJoin is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupSourceEtcdPreparedForPeerJoin flow.TaskID = "TaskGroupSourceEtcdPreparedForPeerJoin"

// sourceEtcdPreparedForPeerJoinTaskGroup returns the flow.TaskGroup for making the source etcd ready for peer join.
func sourceEtcdPreparedForPeerJoinTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	var (
		g = flow.NewTaskGroup(taskGroupSourceEtcdPreparedForPeerJoin).
			SkipIf(v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationMigrateSourceSeedComponents) != nil)

		deployEtcdPeerExposure = g.Add(flow.Task{
			Name: "Deploying etcd member exposure",
			Fn:   b.DeployEtcdMemberExposure,
		})
		persistShootState = g.Add(flow.Task{
			Name: "Persisting shoot state",
			Fn: func(ctx context.Context) error {
				return b.PersistShootState(ctx)
			},
			Dependencies: flow.NewTaskIDs(deployEtcdPeerExposure),
		})
		initSecretsManagement = g.Add(flow.Task{
			Name:         "Initializing secrets management",
			Fn:           b.InitializeSecretsManagement,
			Dependencies: flow.NewTaskIDs(persistShootState),
		})
		deployEtcd = g.Add(flow.Task{
			Name:         "Deploying etcd",
			Fn:           b.DeployEtcd,
			Dependencies: flow.NewTaskIDs(initSecretsManagement),
		})
		waitEtcdsReady = g.Add(flow.Task{
			Name:         "Waiting until etcds are ready",
			Fn:           b.WaitUntilEtcdsReady,
			Dependencies: flow.NewTaskIDs(deployEtcd),
		})

		// The destination restores the `core.BackupEntry` in DestinationEtcdPeersJoinedTaskGroup. Skip
		// re-migrating after the execution of that group has started so that the restore operation done by the
		// destination is not overwritten by a migrate operation done by the source.
		destinationEtcdPeersJoinedConditionExists = v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationDestinationEtcdPeersJoined) != nil

		migrateBackupEntry = g.Add(flow.Task{
			Name:         "Migrating backup entry",
			Fn:           b.Shoot.Components.BackupEntry.Migrate,
			SkipIf:       destinationEtcdPeersJoinedConditionExists,
			Dependencies: flow.NewTaskIDs(waitEtcdsReady),
		})
		_ = g.Add(flow.Task{
			Name:         "Waiting until backup entry has been migrated",
			Fn:           b.Shoot.Components.BackupEntry.WaitMigrate,
			SkipIf:       destinationEtcdPeersJoinedConditionExists,
			Dependencies: flow.NewTaskIDs(migrateBackupEntry),
		})
	)

	return g
}

// taskGroupDestinationEtcdPeersJoined is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupDestinationEtcdPeersJoined flow.TaskID = "TaskGroupDestinationEtcdPeersJoined"

// destinationEtcdPeersJoinedTaskGroup returns the flow.TaskGroup for joining the destination etcd to the source cluster.
func destinationEtcdPeersJoinedTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	// Once etcd migration is complete, the etcd peer-join work (backup entry restore, etcd deployment) must
	// not be re-run. The setup tasks (namespace, peer exposure, secrets management) are still needed on every
	// reconcile so the in-memory secrets manager is populated for subsequent task groups.
	etcdMigrationComplete := v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete) != nil

	var (
		g = flow.NewTaskGroup(taskGroupDestinationEtcdPeersJoined)

		// Fetch the ShootState written by the source so InitializeSecretsManagement can restore from it.
		refreshShootState = g.Add(flow.Task{
			Name: "Refreshing shoot state",
			Fn:   b.RefreshShootStateForLiveMigration,
		})
		deployControlPlaneNamespace = g.Add(flow.Task{
			Name: "Deploying control plane namespace",
			Fn:   b.DeployControlPlaneNamespace,
		})
		deployEtcdPeerExposure = g.Add(flow.Task{
			Name:         "Deploying etcd member exposure",
			Fn:           b.DeployEtcdMemberExposure,
			Dependencies: flow.NewTaskIDs(deployControlPlaneNamespace),
		})
		initSecretsManagement = g.Add(flow.Task{
			Name:         "Initializing secrets management",
			Fn:           b.InitializeSecretsManagement,
			Dependencies: flow.NewTaskIDs(refreshShootState, deployEtcdPeerExposure),
		})
		// The source backup entry is deployed to ensure that the data in the source seed's backup bucket
		// is properly cleaned up at a later stage of the flow.
		deploySourceBackupEntry = g.Add(flow.Task{
			Name:         "Deploying source backup entry",
			Fn:           b.DeploySourceBackupEntry,
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(deployControlPlaneNamespace),
		})
		waitSourceBackupEntryReady = g.Add(flow.Task{
			Name:         "Waiting until source backup entry is ready",
			Fn:           b.Shoot.Components.SourceBackupEntry.Wait,
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(deploySourceBackupEntry),
		})
		restoreBackupEntry = g.Add(flow.Task{
			Name: "Restoring backup entry",
			Fn: func(ctx context.Context) error {
				return b.Shoot.Components.BackupEntry.Restore(ctx, nil)
			},
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(waitSourceBackupEntryReady),
		})
		waitBackupEntryReady = g.Add(flow.Task{
			Name:         "Waiting until backup entry is ready",
			Fn:           b.Shoot.Components.BackupEntry.Wait,
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(restoreBackupEntry),
		})
		deployEtcd = g.Add(flow.Task{
			Name: "Deploying destination etcd to join the source etcd cluster",
			Fn: func(ctx context.Context) error {
				return b.DeployEtcdForLiveMigrationPhase(ctx, 2)
			},
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(initSecretsManagement, deployEtcdPeerExposure, waitBackupEntryReady),
		})
		_ = g.Add(flow.Task{
			Name:         "Waiting until etcds are ready",
			Fn:           b.WaitUntilEtcdsReady,
			SkipIf:       etcdMigrationComplete,
			Dependencies: flow.NewTaskIDs(deployEtcd),
		})
	)

	return g
}

// taskGroupDestinationKubeAPIServerReady is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupDestinationKubeAPIServerReady flow.TaskID = "TaskGroupDestinationKubeAPIServerReady"

// destinationKubeAPIServerReadyTaskGroup brings up the destination control plane and the temporary VPN.
func destinationKubeAPIServerReadyTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	g := flow.NewTaskGroup(taskGroupDestinationKubeAPIServerReady).
		SkipIf(v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationMigrationCompleted) != nil)

	refreshShootState := g.Add(flow.Task{
		Name: "Refreshing shoot state",
		Fn:   b.RefreshShootStateForLiveMigration,
	})
	deployVPNServer := g.Add(flow.Task{
		Name:         "Deploying destination VPN server",
		Fn:           b.DeployVPNServer,
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(refreshShootState),
	})
	// The istio ConfigMap gates the pod-kube-apiserver-load-balancing webhook; without it control plane pods resolve
	// api.<internalDomain> to the source apiserver and fail authentication (401).
	reconcileIstioConfigMap := g.Add(flow.Task{
		Name:         "Reconciling istio internal load balancing configmap",
		Fn:           b.ReconcileIstioInternalLoadBalancingConfigMap,
		Dependencies: flow.NewTaskIDs(refreshShootState),
	})

	deployReferencedResources := b.ReconcileReferencedResourcesTaskGroup()
	g = g.AddAll(deployReferencedResources.TasksWithDependencies(refreshShootState)...)

	deployCloudProviderSecret := b.DeployCloudProviderSecretTaskGroup()
	g = g.AddAll(deployCloudProviderSecret.TasksWithDependencies(deployReferencedResources)...)

	waitUntilExtensionResourcesBeforeKAPIReady := b.ReconcileExtensionsBeforeKubeAPIServerTaskGroup(false)
	g = g.AddAll(waitUntilExtensionResourcesBeforeKAPIReady.TasksWithDependencies(reconcileIstioConfigMap, deployCloudProviderSecret)...)

	deployService := g.Add(flow.Task{
		Name:         "Deploying kube-apiserver service",
		Fn:           b.Shoot.Components.ControlPlane.KubeAPIServerService.Deploy,
		Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesBeforeKAPIReady),
	})
	waitUntilKubeAPIServerServiceIsReady := g.Add(flow.Task{
		Name:         "Waiting until kube-apiserver service is ready",
		Fn:           b.Shoot.Components.ControlPlane.KubeAPIServerService.Wait,
		Dependencies: flow.NewTaskIDs(deployService),
	})

	reconcileKubeAPIServer := b.ReconcileKubeAPIServerTaskGroup(false, 30*time.Second)
	g = g.AddAll(reconcileKubeAPIServer.TasksWithDependencies(waitUntilKubeAPIServerServiceIsReady, deployVPNServer)...)

	waitUntilGardenerResourceManagerReady := b.ReconcileGardenerResourceManagerTaskGroup(true, false)
	g = g.AddAll(waitUntilGardenerResourceManagerReady.TasksWithDependencies(reconcileKubeAPIServer)...)

	deployControlPlaneComponents := b.DeployControlPlaneComponentsTaskGroup(false)
	g = g.AddAll(deployControlPlaneComponents.TasksWithDependencies(waitUntilGardenerResourceManagerReady)...)

	// TODO(GEP-39): Deploy temporary VPN shoot client and live migration VPN DNS record as part of the live migration process.

	deployGardenerAccess := g.Add(flow.Task{
		Name:         "Deploying Gardener shoot access resources",
		Fn:           flow.TaskFn(b.Shoot.Components.GardenerAccess.Deploy).RetryUntilTimeout(5*time.Second, 30*time.Second),
		Dependencies: flow.NewTaskIDs(waitUntilGardenerResourceManagerReady),
	})
	// WaitUntilTemporaryTunnelConnectionExists lists pods through b.ShootClientSet, which is nil until the shoot client
	// is initialized against the freshly deployed destination kube-apiserver.
	_ = g.Add(flow.Task{
		Name:         "Initializing connection to shoot",
		Fn:           flow.TaskFn(b.InitializeDesiredShootClients).RetryUntilTimeout(5*time.Second, 2*time.Minute),
		Dependencies: flow.NewTaskIDs(deployGardenerAccess),
	})

	// TODO(GEP-39): Wait until the temporary VPN tunnel connection to the destination is established.

	return g
}

// taskGroupMigrateSourceSeedComponents is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupMigrateSourceSeedComponents flow.TaskID = "TaskGroupMigrateSourceSeedComponents"

// migrateSourceSeedComponentsTaskGroup migrates and tears down the source-side shoot resources (extensions, infrastructure,
// control plane, managed resources, machines) and persists the migrated state for the destination to restore.
func migrateSourceSeedComponentsTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	var (
		g = flow.NewTaskGroup(taskGroupMigrateSourceSeedComponents).
			SkipIf(v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationEtcdMigrationComplete) != nil)
	)

	migrateDNSRecords := b.MigrateDNSRecordsTaskGroup()
	g = g.AddAll(migrateDNSRecords.TasksWithDependencies()...)

	waitForManagedResourcesDeletion := b.MigrateAndDeleteManagedResourcesTaskGroup(true)
	g = g.AddAll(waitForManagedResourcesDeletion.TasksWithDependencies(migrateDNSRecords)...)

	waitUntilMachineControllerManagerDeleted := b.DeleteMachineControllerManagerTaskGroup()
	g = g.AddAll(waitUntilMachineControllerManagerDeleted.TasksWithDependencies(waitForManagedResourcesDeletion)...)

	waitUntilExtensionResourcesMigrated := b.MigrateExtensionResourcesTaskGroup()
	g = g.AddAll(waitUntilExtensionResourcesMigrated.TasksWithDependencies(waitUntilMachineControllerManagerDeleted)...)

	persistShootState := g.Add(flow.Task{
		Name:         "Persisting shoot state",
		Fn:           b.PersistShootState,
		Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesMigrated),
	})

	waitUntilStaleExtensionResourcesDeleted := b.DestroyExtensionsAndMachineResourcesTaskGroup()
	g = g.AddAll(waitUntilStaleExtensionResourcesDeleted.TasksWithDependencies(persistShootState)...)

	waitUntilControlPlaneDeleted := b.MigrateAndDeleteControlPlaneTaskGroup()
	g = g.AddAll(waitUntilControlPlaneDeleted.TasksWithDependencies(waitUntilStaleExtensionResourcesDeleted)...)

	waitUntilExtensionsAfterKubeAPIServerDeleted := b.MigrateAndDeleteExtensionsAfterKubeAPIServerTaskGroup()
	g = g.AddAll(waitUntilExtensionsAfterKubeAPIServerDeleted.TasksWithDependencies(waitUntilControlPlaneDeleted)...)

	_ = g.Add(flow.Task{
		Name:         "Waiting until all extensions have been deleted",
		Fn:           b.Shoot.Components.Extensions.Extension.WaitCleanup,
		Dependencies: flow.NewTaskIDs(waitUntilExtensionsAfterKubeAPIServerDeleted),
	})

	waitUntilInfrastructureDeleted := b.MigrateAndDeleteInfrastructureTaskGroup()
	g = g.AddAll(waitUntilInfrastructureDeleted.TasksWithDependencies(waitUntilExtensionsAfterKubeAPIServerDeleted)...)

	return g
}

// taskGroupEtcdMigrationComplete is a flow.TaskID for the etcd migration completion task group.
const taskGroupEtcdMigrationComplete flow.TaskID = "TaskGroupEtcdMigrationComplete"

// etcdMigrationCompleteTaskGroup returns the flow.TaskGroup for completing the etcd migration.
// It first deploys all destination DNS records and waits for DNS caches to propagate, then scales
// out the destination etcd to 3 replicas, removes source members, cleans up the live migration
// configuration, and takes a full snapshot.
// DNS is deployed here — before source etcd members are removed — so the source kube-apiserver can
// only go down after all DNS caches have already switched to the destination.
func etcdMigrationCompleteTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	var (
		skipIfSourceMembersAlreadyRemoved = func(fn flow.TaskFn) flow.TaskFn {
			return func(ctx context.Context) error {
				sourceMembersAlreadyRemoved, err := b.IsSourceMembersAlreadyRemoved(ctx)
				if err != nil || sourceMembersAlreadyRemoved {
					return err
				}
				return fn(ctx)
			}
		}

		g = flow.NewTaskGroup(taskGroupEtcdMigrationComplete).
			SkipIf(v1beta1helper.GetLiveMigrationCondition(b.Shoot.GetInfo(), gardencorev1beta1.ShootLiveMigrationSourceSeedFinalTeardown) != nil)
	)

	// Deploy all DNS records pointing to the destination before scaling out etcd, so the source
	// kube-apiserver can only go down after DNS records have already switched to the destination.
	reconcileDNSRecords := b.ReconcileDNSRecordsTaskGroup()
	g = g.AddAll(reconcileDNSRecords.TasksWithDependencies()...)

	waitDNSCacheSync := g.Add(flow.Task{
		Name:         "Waiting for DNS caches to sync to the destination",
		Fn:           skipIfSourceMembersAlreadyRemoved(b.WaitForLiveMigrationDNSCacheSync),
		Dependencies: flow.NewTaskIDs(reconcileDNSRecords),
	})
	deployEtcd := g.Add(flow.Task{
		Name:         "Scaling out destination etcd to 3 replicas",
		Fn:           skipIfSourceMembersAlreadyRemoved(b.DeployEtcd),
		Dependencies: flow.NewTaskIDs(waitDNSCacheSync),
	})
	waitUntilEtcdReady := g.Add(flow.Task{
		Name:         "Waiting until etcd is scaled out and ready",
		Fn:           skipIfSourceMembersAlreadyRemoved(b.WaitUntilEtcdsReady),
		Dependencies: flow.NewTaskIDs(deployEtcd),
	})
	removeSourceMembers := g.Add(flow.Task{
		Name:         "Removing source etcd members from the joined cluster",
		Fn:           skipIfSourceMembersAlreadyRemoved(b.RemoveSourceEtcdMembers),
		Dependencies: flow.NewTaskIDs(waitUntilEtcdReady),
	})
	waitUntilSourceMembersRemoved := g.Add(flow.Task{
		Name:         "Waiting until source etcd members are removed",
		Fn:           b.WaitUntilSourceEtcdMembersRemoved,
		Dependencies: flow.NewTaskIDs(removeSourceMembers),
	})
	deployEtcdWithoutLiveMigration := g.Add(flow.Task{
		Name: "Removing live migration configuration from etcd",
		Fn: func(ctx context.Context) error {
			b.Shoot.Components.ControlPlane.EtcdMain.SetLiveMigration(nil)
			return b.DeployEtcd(ctx)
		},
		Dependencies: flow.NewTaskIDs(waitUntilSourceMembersRemoved),
	})
	waitUntilEtcdReadyAfterCleanup := g.Add(flow.Task{
		Name:         "Waiting until etcds are ready after cleanup",
		Fn:           b.WaitUntilEtcdsReady,
		Dependencies: flow.NewTaskIDs(deployEtcdWithoutLiveMigration),
	})
	_ = g.Add(flow.Task{
		Name:         "Destroying ETCD member exposure",
		Fn:           b.DestroyEtcdMemberExposure,
		Dependencies: flow.NewTaskIDs(waitUntilEtcdReadyAfterCleanup),
	})
	_ = g.Add(flow.Task{
		Name:         "Taking full etcd snapshot",
		Fn:           b.SnapshotEtcd,
		SkipIf:       v1beta1helper.GetBackupConfigForShoot(b.Shoot.GetInfo(), b.GetSeed()) == nil,
		Dependencies: flow.NewTaskIDs(waitUntilEtcdReadyAfterCleanup),
	})

	return g
}

// taskGroupDestinationCompletion is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupDestinationCompletion flow.TaskID = "TaskGroupDestinationCompletion"

// destinationCompletionTaskGroup returns the group run by the MigrationCompleted step: the restore chain followed by
// the destination shoot-system layer (system components, resources, addons, observability), run sequentially.
func destinationCompletionTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	var (
		kubeProxyEnabled = v1beta1helper.KubeProxyEnabled(b.Shoot.GetInfo().Spec.Kubernetes.KubeProxy)
		nginxDisabled    = b.Shoot.IsWorkerless || !v1beta1helper.NginxIngressEnabled(b.Shoot.GetInfo().Spec.Addons)
		g                = flow.NewTaskGroup(taskGroupDestinationCompletion)
	)

	refreshShootState := g.Add(flow.Task{
		Name: "Refreshing shoot state",
		Fn:   b.RefreshShootStateForLiveMigration,
	})
	// The kube-apiserver service was deployed and confirmed ready in destinationKubeAPIServerReadyTaskGroup,
	// so its external IP is available. Update advertised addresses early so they are correct for the rest
	// of the flow (matches the reconciler which runs this right after kube-apiserver service is ready).
	_ = g.Add(flow.Task{
		Name:         "Ensuring advertised addresses for the shoot",
		Fn:           b.UpdateAdvertisedAddresses,
		Dependencies: flow.NewTaskIDs(refreshShootState),
	})

	deployReferencedResources := b.ReconcileReferencedResourcesTaskGroup()
	g = g.AddAll(deployReferencedResources.TasksWithDependencies(refreshShootState)...)

	deployCloudProviderSecret := b.DeployCloudProviderSecretTaskGroup()
	g = g.AddAll(deployCloudProviderSecret.TasksWithDependencies(deployReferencedResources)...)

	waitUntilInfrastructureReady := b.ReconcileInfrastructureTaskGroup(false)
	g = g.AddAll(waitUntilInfrastructureReady.TasksWithDependencies(deployCloudProviderSecret)...)

	waitUntilControlPlaneReady := b.ReconcileControlPlaneTaskGroup(false)
	g = g.AddAll(waitUntilControlPlaneReady.TasksWithDependencies(waitUntilInfrastructureReady)...)

	restoreNetwork := g.Add(flow.Task{
		Name:         "Restoring network",
		Fn:           b.DeployNetwork,
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(waitUntilControlPlaneReady),
	})
	waitNetwork := g.Add(flow.Task{
		Name:         "Waiting until network is restored",
		Fn:           flow.TaskFn(func(ctx context.Context) error { return b.Shoot.Components.Extensions.Network.Wait(ctx) }),
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(restoreNetwork),
	})

	waitUntilOperatingSystemConfigReady := b.ReconcileOperatingSystemConfigTaskGroup()
	g = g.AddAll(waitUntilOperatingSystemConfigReady.TasksWithDependencies(waitNetwork)...)

	// machine-controller-manager must run before the worker is restored so it adopts the migrated MachineDeployments.
	waitUntilMachineControllerManagerReady := b.ReconcileMachineControllerManagerTaskGroup()
	g = g.AddAll(waitUntilMachineControllerManagerReady.TasksWithDependencies(waitUntilOperatingSystemConfigReady)...)

	waitUntilWorkerReady := b.ReconcileWorkerTaskGroup(false)
	g = g.AddAll(waitUntilWorkerReady.TasksWithDependencies(waitUntilMachineControllerManagerReady)...)

	g = g.AddAll(b.DeleteStaleOperatingSystemConfigResourcesTaskGroup(false).TasksWithDependencies(waitUntilWorkerReady)...)

	waitUntilExtensionResourcesAfterWorkerReady := b.ReconcileExtensionsAfterWorkerTaskGroup(false)
	g = g.AddAll(waitUntilExtensionResourcesAfterWorkerReady.TasksWithDependencies(waitUntilWorkerReady)...)

	waitUntilContainerRuntimeReady := b.ReconcileContainerRuntimeTaskGroup(false)
	g = g.AddAll(waitUntilContainerRuntimeReady.TasksWithDependencies(waitUntilExtensionResourcesAfterWorkerReady)...)

	waitUntilExtensionResourcesAfterKAPIReady := b.ReconcileExtensionsAfterKubeAPIServerTaskGroup(false)
	g = g.AddAll(waitUntilExtensionResourcesAfterKAPIReady.TasksWithDependencies(waitUntilContainerRuntimeReady)...)

	// Deploy the permanent vpn-shoot and wait until it is healthy before tearing down the temporary VPN, so the
	// permanent tunnel to the destination is established first and connectivity is uninterrupted.
	deployVPNShoot := g.Add(flow.Task{
		Name:         "Deploying VPN shoot client to destination",
		Fn:           b.DeployVPNShoot,
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesAfterKAPIReady),
	})
	waitVPNShoot := g.Add(flow.Task{
		Name:         "Waiting until VPN shoot client to destination is healthy",
		Fn:           flow.TaskFn(func(ctx context.Context) error { return b.Shoot.Components.SystemComponents.VPNShoot.Wait(ctx) }),
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(deployVPNShoot),
	})
	// Re-deploying GardenerAccess here (it was also deployed in destinationKubeAPIServerReadyTaskGroup for the
	// temporary tunnel) ensures the access resources are refreshed before InitializeDesiredShootClients re-establishes
	// the connection through the permanent VPN tunnel.
	deployGardenerAccess := g.Add(flow.Task{
		Name:         "Deploying Gardener access resources",
		Fn:           b.Shoot.Components.GardenerAccess.Deploy,
		Dependencies: flow.NewTaskIDs(waitVPNShoot),
	})
	initializeShootClients := g.Add(flow.Task{
		Name:         "Initializing connection to shoot",
		Fn:           b.InitializeDesiredShootClients,
		Dependencies: flow.NewTaskIDs(deployGardenerAccess),
	})
	// Confirm the permanent tunnel actually carries traffic to the shoot BEFORE tearing down the
	// temporary VPN. Otherwise the temp path is removed while the permanent one may not yet be
	// established end-to-end (VPNShoot.Wait only checks pod/ManagedResource health, not tunnel
	// reachability), briefly black-holing apiserver->shoot traffic - webhooks, logs, exec.
	waitTunnelConnection := g.Add(flow.Task{
		Name:         "Waiting until the permanent VPN tunnel connection to the destination is established",
		Fn:           b.WaitUntilTunnelConnectionExists,
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(initializeShootClients),
	})

	// TODO(GEP-39): Destroy the temporary VPN shoot client and live migration VPN DNS record after the permanent tunnel is established.

	_ = g.Add(flow.Task{
		Name: "Waiting until all shoot worker nodes have updated the operating system config",
		Fn: func(ctx context.Context) error {
			return b.WaitUntilOperatingSystemConfigUpdatedForAllWorkerPools(ctx, false, false)
		},
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(waitTunnelConnection),
	})
	// EnsureShootClusterIdentity populates status.ClusterIdentity, read by the system-components group, so it runs first.
	deployAPIServerProxy := g.Add(flow.Task{
		Name:         "Deploying apiserver-proxy",
		Fn:           b.DeployAPIServerProxy,
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(waitTunnelConnection),
	})
	ensureShootClusterIdentity := g.Add(flow.Task{
		Name:         "Ensuring shoot cluster identity",
		Fn:           b.EnsureShootClusterIdentity,
		Dependencies: flow.NewTaskIDs(deployAPIServerProxy),
	})

	waitUntilShootNamespacesReady := b.ReconcileShootNamespacesTaskGroup(false)
	g = g.AddAll(waitUntilShootNamespacesReady.TasksWithDependencies(ensureShootClusterIdentity)...)

	waitUntilSystemComponentsReady := b.ReconcileSystemComponentsTaskGroup(kubeProxyEnabled, false)
	g = g.AddAll(waitUntilSystemComponentsReady.TasksWithDependencies(waitUntilShootNamespacesReady)...)

	waitUntilSystemResourcesReady := b.ReconcileSystemResourcesTaskGroup()
	g = g.AddAll(waitUntilSystemResourcesReady.TasksWithDependencies(waitUntilSystemComponentsReady)...)

	deployNodeAgent := g.Add(flow.Task{
		Name: "Deploying gardener-node-agent's OperatingSystemConfig secrets and RBAC resources",
		Fn: func(ctx context.Context) error {
			return b.DeployManagedResourceForGardenerNodeAgent(ctx, false)
		},
		SkipIf:       b.Shoot.IsWorkerless,
		Dependencies: flow.NewTaskIDs(waitUntilSystemResourcesReady),
	})
	populateStaticManifests := g.Add(flow.Task{
		Name:         "Populating static manifests from seed to shoot",
		Fn:           b.PopulateStaticManifestsFromSeedToShoot,
		Dependencies: flow.NewTaskIDs(deployNodeAgent),
	})

	deployLogging := g.Add(flow.Task{
		Name:         "Deploying shoot logging stack in seed",
		Fn:           b.DeployLogging,
		Dependencies: flow.NewTaskIDs(populateStaticManifests),
	})
	reconcileObservabilityComponents := b.ReconcileObservabilityTaskGroup()
	g = g.AddAll(reconcileObservabilityComponents.TasksWithDependencies(deployLogging)...)

	waitNginx := g.Add(flow.Task{
		Name:         "Waiting until nginx ingress LoadBalancer is ready",
		Fn:           b.WaitUntilNginxIngressServiceIsReady,
		SkipIf:       nginxDisabled,
		Dependencies: flow.NewTaskIDs(reconcileObservabilityComponents),
	})
	_ = g.Add(flow.Task{
		Name:         "Deploying nginx ingress DNS record",
		Fn:           b.DeployOrDestroyIngressDNSRecord,
		SkipIf:       nginxDisabled,
		Dependencies: flow.NewTaskIDs(waitNginx),
	})

	return g
}

// taskGroupSourceSeedFinalTeardown is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupSourceSeedFinalTeardown flow.TaskID = "TaskGroupSourceSeedFinalTeardown"

// sourceSeedFinalTeardownTaskGroup tears down the remaining source-seed resources that can only be removed after
// the etcd migration is complete: source etcd, DNS records, and the source backup entry (which depended on the
// migrated infrastructure). It also removes the VPN server and finally the shoot namespace itself.
func sourceSeedFinalTeardownTaskGroup(b *botanistpkg.Botanist, o *operation.Operation) flow.TaskGroup {
	var (
		skipIfAbsent = func(fn flow.TaskFn) flow.TaskFn {
			return func(ctx context.Context) error {
				if b.SeedNamespaceObject == nil || b.SeedNamespaceObject.UID == "" {
					return nil
				}
				return fn(ctx)
			}
		}

		g = flow.NewTaskGroup(taskGroupSourceSeedFinalTeardown)
	)

	checkNamespace := g.Add(flow.Task{
		Name: "Checking if shoot namespace exists in source seed",
		Fn: func(ctx context.Context) error {
			return checkIfSeedNamespaceExists(ctx, o, b)
		},
	})
	destroyEtcd := g.Add(flow.Task{
		Name:         "Destroying source etcd",
		Fn:           skipIfAbsent(flow.TaskFn(b.DestroyEtcd).RetryUntilTimeout(defaultInterval, defaultTimeout)),
		Dependencies: flow.NewTaskIDs(checkNamespace),
	})
	waitUntilEtcdDeleted := g.Add(flow.Task{
		Name:         "Waiting until source etcd has been destroyed",
		Fn:           skipIfAbsent(flow.TaskFn(b.WaitUntilEtcdsDeleted).RetryUntilTimeout(defaultInterval, defaultTimeout)),
		Dependencies: flow.NewTaskIDs(destroyEtcd),
	})
	destroyEtcdMemberExposure := g.Add(flow.Task{
		Name:         "Destroying ETCD member exposure",
		Fn:           skipIfAbsent(b.DestroyEtcdMemberExposure),
		Dependencies: flow.NewTaskIDs(checkNamespace),
	})
	destroyDNSRecords := g.Add(flow.Task{
		Name:         "Deleting DNSRecords from the source namespace",
		Fn:           skipIfAbsent(b.DestroyDNSRecords),
		Dependencies: flow.NewTaskIDs(checkNamespace),
	})
	destroySourceBackupEntry := g.Add(flow.Task{
		Name:         "Deleting source backup entry from the source namespace",
		Fn:           skipIfAbsent(b.DestroySourceBackupEntry),
		Dependencies: flow.NewTaskIDs(checkNamespace),
	})
	destroyVPNServer := g.Add(flow.Task{
		Name:         "Deleting vpn-seed-server from the source namespace",
		Fn:           skipIfAbsent(b.DestroyVPNServer),
		Dependencies: flow.NewTaskIDs(checkNamespace),
		SkipIf:       b.Shoot.IsWorkerless,
	})
	deleteNamespace := g.Add(flow.Task{
		Name:         "Deleting shoot namespace in Seed",
		Fn:           flow.TaskFn(b.DeleteSeedNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
		Dependencies: flow.NewTaskIDs(waitUntilEtcdDeleted, destroyEtcdMemberExposure, destroyDNSRecords, destroySourceBackupEntry, destroyVPNServer),
	})
	waitUntilNamespaceDeleted := g.Add(flow.Task{
		Name:         "Waiting until shoot namespace in Seed has been deleted",
		Fn:           b.WaitUntilSeedNamespaceDeleted,
		Dependencies: flow.NewTaskIDs(deleteNamespace),
	})
	_ = g.Add(flow.Task{
		Name: "Deleting Cluster resource from source seed",
		Fn: func(ctx context.Context) error {
			return o.DeleteClusterResourceFromSeed(ctx)
		},
		Dependencies: flow.NewTaskIDs(waitUntilNamespaceDeleted),
	})
	return g
}

// taskGroupAbortDestinationCleanup is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupAbortDestinationCleanup flow.TaskID = "TaskGroupAbortDestinationCleanup"

// abortDestinationCleanupTaskGroup tears down the destination seed's migration-specific resources when a live
// migration is aborted. Destroying the destination etcd deletes the etcd CR, which makes etcd-druid remove the members
// it added (to join the source cluster) before the StatefulSet is torn down - this runs before the namespace deletion
// so the removal happens gracefully while the source peer endpoints are still reachable. All tasks are skipped if the
// destination control plane namespace no longer exists (idempotent re-runs).
func abortDestinationCleanupTaskGroup(ctx context.Context, b *botanistpkg.Botanist, o *operation.Operation) flow.TaskGroup {
	if err := checkIfSeedNamespaceExists(ctx, o, b); err != nil {
		b.Logger.Error(err, "Failed to check if seed namespace exists, assuming it exists")
	}
	namespaceExists := b.SeedNamespaceObject != nil && b.SeedNamespaceObject.UID != ""

	g := flow.NewTaskGroup(taskGroupAbortDestinationCleanup)

	destroyEtcd := g.Add(flow.Task{
		Name:   "Destroying destination etcd and removing its members from the cluster",
		Fn:     flow.TaskFn(b.DestroyEtcd).RetryUntilTimeout(defaultInterval, defaultTimeout),
		SkipIf: !namespaceExists,
	})
	waitUntilEtcdDeleted := g.Add(flow.Task{
		Name:         "Waiting until destination etcd has been destroyed",
		Fn:           flow.TaskFn(b.WaitUntilEtcdsDeleted).RetryUntilTimeout(defaultInterval, defaultTimeout),
		Dependencies: flow.NewTaskIDs(destroyEtcd),
		SkipIf:       !namespaceExists,
	})
	destroyEtcdMemberExposure := g.Add(flow.Task{
		Name:         "Destroying destination etcd member exposure",
		Fn:           b.DestroyEtcdMemberExposure,
		Dependencies: flow.NewTaskIDs(waitUntilEtcdDeleted),
		SkipIf:       !namespaceExists,
	})
	destroySourceBackupEntry := g.Add(flow.Task{
		Name:         "Deleting source backup entry from the destination namespace",
		Fn:           b.DestroySourceBackupEntry,
		Dependencies: flow.NewTaskIDs(waitUntilEtcdDeleted),
		SkipIf:       !namespaceExists,
	})
	deleteNamespace := g.Add(flow.Task{
		Name:         "Deleting shoot namespace in destination Seed",
		Fn:           flow.TaskFn(b.DeleteSeedNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
		Dependencies: flow.NewTaskIDs(destroyEtcdMemberExposure, destroySourceBackupEntry),
		SkipIf:       !namespaceExists,
	})
	_ = g.Add(flow.Task{
		Name:         "Waiting until shoot namespace in destination Seed has been deleted",
		Fn:           b.WaitUntilSeedNamespaceDeleted,
		Dependencies: flow.NewTaskIDs(deleteNamespace),
		SkipIf:       !namespaceExists,
	})
	return g
}

// taskGroupAbortSourceRevert is a flow.TaskID for a logical flow.TaskGroup.
const taskGroupAbortSourceRevert flow.TaskID = "TaskGroupAbortSourceRevert"

// abortSourceRevertTaskGroup reverts the source seed's etcd to a standalone cluster when a live migration is aborted:
// it clears the cross-seed live-migration etcd configuration (peer/client URLs, bootstrap, SAN skip) and redeploys the
// etcd, removes the source etcd member exposure, and restores the shoot's backup entry ownership to the source seed.
// It runs only after the destination cleanup has completed so the destination can deregister its members first.
func abortSourceRevertTaskGroup(b *botanistpkg.Botanist) flow.TaskGroup {
	g := flow.NewTaskGroup(taskGroupAbortSourceRevert)

	deployEtcdWithoutLiveMigration := g.Add(flow.Task{
		Name: "Removing live migration configuration from source etcd",
		Fn: func(ctx context.Context) error {
			b.Shoot.Components.ControlPlane.EtcdMain.SetLiveMigration(nil)
			return b.DeployEtcd(ctx)
		},
	})
	waitUntilEtcdReady := g.Add(flow.Task{
		Name:         "Waiting until source etcd is ready after reverting live migration configuration",
		Fn:           b.WaitUntilEtcdsReady,
		Dependencies: flow.NewTaskIDs(deployEtcdWithoutLiveMigration),
	})
	_ = g.Add(flow.Task{
		Name:         "Destroying source etcd member exposure",
		Fn:           b.DestroyEtcdMemberExposure,
		Dependencies: flow.NewTaskIDs(waitUntilEtcdReady),
	})
	_ = g.Add(flow.Task{
		Name: "Restoring backup entry ownership to the source seed",
		Fn: func(ctx context.Context) error {
			return b.Shoot.Components.BackupEntry.Restore(ctx, nil)
		},
		Dependencies: flow.NewTaskIDs(waitUntilEtcdReady),
	})
	return g
}

// checkGardenletVersionParity returns an error when the local and peer gardenlet versions differ or are not reported.
func (r *Reconciler) checkGardenletVersionParity(ctx context.Context, shoot *gardencorev1beta1.Shoot, role v1beta1helper.LiveMigrationRole) error {
	if r.Config.SeedConfig == nil {
		return fmt.Errorf("seed config is not set")
	}
	ownSeedName := r.Config.SeedConfig.Name

	localVersion := r.Identity.Version
	if localVersion == "" {
		return newGardenletVersionNotReportedError(role, ownSeedName)
	}

	peerSeedName, peerRole := liveMigratePeer(shoot, ownSeedName)
	if peerRole == v1beta1helper.LiveMigrationRoleNone {
		// The shoot is no longer undergoing a live migration (e.g. it was aborted).
		return nil
	}

	peerSeed := &gardencorev1beta1.Seed{}
	if err := r.GardenAPIReader.Get(ctx, client.ObjectKey{Name: peerSeedName}, peerSeed); err != nil {
		return fmt.Errorf("getting live migration peer seed %q: %w", peerSeedName, err)
	}

	peerVersion := ptr.Deref(peerSeed.Status.Gardener, gardencorev1beta1.Gardener{}).Version
	if peerVersion == "" {
		return newGardenletVersionNotReportedError(peerRole, peerSeedName)
	}

	if localVersion != peerVersion {
		sourceVersion, destinationVersion := localVersion, peerVersion
		if role == v1beta1helper.LiveMigrationRoleDestination {
			sourceVersion, destinationVersion = destinationVersion, sourceVersion
		}
		return fmt.Errorf("source and destination seeds must run the same gardenlet version (source: %q, destination: %q)", sourceVersion, destinationVersion)
	}

	return nil
}

// newGardenletVersionNotReportedError returns an error for a seed whose gardenlet version is not reported.
func newGardenletVersionNotReportedError(role v1beta1helper.LiveMigrationRole, seedName string) error {
	return fmt.Errorf("gardenlet version not reported for %s seed %q", strings.ToLower(string(role)), seedName)
}

func liveMigratePeer(shoot *gardencorev1beta1.Shoot, ownSeedName string) (seedName string, role v1beta1helper.LiveMigrationRole) {
	switch v1beta1helper.GetLiveMigrationRole(shoot, ownSeedName) {
	case v1beta1helper.LiveMigrationRoleSource:
		return ptr.Deref(shoot.Spec.SeedName, ""), v1beta1helper.LiveMigrationRoleDestination
	case v1beta1helper.LiveMigrationRoleDestination:
		return ptr.Deref(shoot.Status.SeedName, ""), v1beta1helper.LiveMigrationRoleSource
	default:
		return "", v1beta1helper.LiveMigrationRoleNone
	}
}
