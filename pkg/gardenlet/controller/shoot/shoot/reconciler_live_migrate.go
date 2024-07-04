// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package shoot

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	v1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/gardenlet/operation"
	botanistpkg "github.com/gardener/gardener/pkg/gardenlet/operation/botanist"
	errorsutils "github.com/gardener/gardener/pkg/utils/errors"
	"github.com/gardener/gardener/pkg/utils/flow"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/gardener/shootstate"
	retryutils "github.com/gardener/gardener/pkg/utils/retry"
)

func (r *Reconciler) runLiveMigrateShootFlow(ctx context.Context, o *operation.Operation) *v1beta1helper.WrappedLastErrors {
	var (
		botanist                     *botanistpkg.Botanist
		err                          error
		tasksWithErrors              []string
		kubeAPIServerDeploymentFound = true
		etcdSnapshotRequired         bool
	)

	for _, lastError := range o.Shoot.GetInfo().Status.LastErrors {
		if lastError.TaskID != nil {
			tasksWithErrors = append(tasksWithErrors, *lastError.TaskID)
		}
	}

	errorContext := errorsutils.NewErrorContext("Shoot cluster preparation for migration", tasksWithErrors)

	err = errorsutils.HandleErrors(errorContext,
		func(errorID string) error {
			o.CleanShootTaskError(ctx, errorID)
			return nil
		},
		nil,
		errorsutils.ToExecute("Create botanist", func() error {
			return retryutils.UntilTimeout(ctx, 10*time.Second, 10*time.Minute, func(context.Context) (done bool, err error) {
				botanist, err = botanistpkg.New(ctx, o)
				if err != nil {
					return retryutils.MinorError(err)
				}
				return retryutils.Ok()
			})
		}),
		errorsutils.ToExecute("Retrieve kube-apiserver deployment in the shoot namespace in the seed cluster", func() error {
			deploymentKubeAPIServer := &appsv1.Deployment{}
			if err := botanist.SeedClientSet.APIReader().Get(ctx, client.ObjectKey{Namespace: o.Shoot.SeedNamespace, Name: v1beta1constants.DeploymentNameKubeAPIServer}, deploymentKubeAPIServer); err != nil {
				if !apierrors.IsNotFound(err) {
					return err
				}
				kubeAPIServerDeploymentFound = false
			}
			if deploymentKubeAPIServer.DeletionTimestamp != nil {
				kubeAPIServerDeploymentFound = false
			}
			return nil
		}),
		errorsutils.ToExecute("Retrieve the Shoot namespace in the Seed cluster", func() error {
			return checkIfSeedNamespaceExists(ctx, o, botanist)
		}),
		errorsutils.ToExecute("Retrieve the BackupEntry in the garden cluster", func() error {
			backupEntry := &gardencorev1beta1.BackupEntry{}
			err := botanist.GardenClient.Get(ctx, client.ObjectKey{Name: botanist.Shoot.BackupEntryName, Namespace: o.Shoot.GetInfo().Namespace}, backupEntry)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return err
				}
				return nil
			}
			etcdSnapshotRequired = backupEntry.Spec.SeedName != nil && *backupEntry.Spec.SeedName == *botanist.Shoot.GetInfo().Status.SeedName && botanist.SeedNamespaceObject.UID != ""
			return nil
		}),
	)

	if err != nil {
		return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), err)
	}

	var (
		nonTerminatingNamespace = botanist.SeedNamespaceObject.UID != "" && botanist.SeedNamespaceObject.Status.Phase != corev1.NamespaceTerminating
		cleanupShootResources   = nonTerminatingNamespace && kubeAPIServerDeploymentFound
		wakeupRequired          = (o.Shoot.GetInfo().Status.IsHibernated || o.Shoot.HibernationEnabled) && cleanupShootResources
		defaultTimeout          = 10 * time.Minute
		defaultInterval         = 5 * time.Second
		etcdMemberRemoved       = metav1.HasAnnotation(o.Shoot.GetInfo().ObjectMeta, v1beta1constants.AnnotationEtcdMemberRemoved)

		g = flow.NewGraph("Shoot cluster preparation for migration")

		deployNamespace = g.Add(flow.Task{
			Name:   "Deploying Shoot namespace in Seed",
			Fn:     flow.TaskFn(botanist.DeploySeedNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf: !nonTerminatingNamespace,
		})
		createServicesAndNetpol = g.Add(flow.Task{
			Name: "Deploying ETCD services and network policies",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := botanist.CreateServicesAndNetpol(ctx, o.Logger, botanist.Shoot.SeedNamespace); err != nil {
					return err
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		copyEtcdBackupSecretInGarden = g.Add(flow.Task{
			Name: "Copying ETCD backup secret in Garden",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := botanist.CopyEtcdBackupSecretInGarden(ctx, o.Logger, botanist.Shoot.SeedNamespace); err != nil {
					return err
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		waitForLBAnnotation = g.Add(flow.Task{
			Name: "Waiting for the shoot.gardener.cloud/target-load-balancer-ips-ready annotation",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationShootTargetLoadBalancerIPsReady)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(createServicesAndNetpol, copyEtcdBackupSecretInGarden),
			SkipIf:       etcdMemberRemoved,
		})
		storeLoadBalancerIPsOfETCDServices = g.Add(flow.Task{
			Name: "Store LoadBalancer IP of ETCD services in Garden cluster",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := botanist.StoreLoadBalancerIPsOfETCDServices(ctx, o.Logger, botanist.Shoot.SeedNamespace); err != nil {
					return err
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(createServicesAndNetpol),
		})
		initializeSecretsManagement = g.Add(flow.Task{
			Name:         "Initializing secrets management",
			Fn:           flow.TaskFn(botanist.InitializeSecretsManagement).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       !nonTerminatingNamespace,
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		deployETCD = g.Add(flow.Task{
			Name: "Deploying main and events etcd",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.DeployEtcd(ctx)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       (!cleanupShootResources && !etcdSnapshotRequired) || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(initializeSecretsManagement, waitForLBAnnotation, storeLoadBalancerIPsOfETCDServices),
		})
		scaleUpETCD = g.Add(flow.Task{
			Name: "Scaling etcd up",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.ScaleUpETCD(ctx)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       !wakeupRequired || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployETCD),
		})
		waitUntilEtcdReady = g.Add(flow.Task{
			Name: "Waiting until main and event etcd report readiness",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.WaitUntilEtcdsReady(ctx)
			}),
			SkipIf:       (!cleanupShootResources && !etcdSnapshotRequired) || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployETCD, scaleUpETCD),
		})
		persistShootState = g.Add(flow.Task{
			Name: "Persisting ShootState in garden cluster",
			Fn: func(ctx context.Context) error {
				return shootstate.Deploy(ctx, r.Clock, botanist.GardenClient, botanist.SeedClientSet.Client(), botanist.Shoot.GetInfo(), false)
			},
			Dependencies: flow.NewTaskIDs(waitUntilEtcdReady),
		})
		annotationSourceEtcdIsReady = g.Add(flow.Task{
			Name: "Annotate shoot that source etcd is ready",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := o.Shoot.UpdateInfo(ctx, o.GardenClient, false, func(shoot *gardencorev1beta1.Shoot) error {
					metav1.SetMetaDataAnnotation(&shoot.ObjectMeta, v1beta1constants.AnnotationSourceEtcdIsReady, "true")
					return nil
				}); err != nil {
					return nil
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(storeLoadBalancerIPsOfETCDServices, persistShootState),
		})
		// till here everthing is done for source etcd.
		waitTargetEtcdIsReady = g.Add(flow.Task{
			Name: "Waiting for the shoot.gardener.cloud/target-etcd-ready annotation",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationTargetEtcdIsReady)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(createServicesAndNetpol),
		})
		wakeUpKubeAPIServer = g.Add(flow.Task{
			Name:         "Scaling Kubernetes API Server up and waiting until ready",
			Fn:           botanist.WakeUpKubeAPIServer,
			SkipIf:       !wakeupRequired || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployETCD, scaleUpETCD, initializeSecretsManagement, annotationSourceEtcdIsReady, waitTargetEtcdIsReady),
		})
		// Deploy gardener-resource-manager to re-run the bootstrap logic if needed (e.g. when the token is expired because of hibernation).
		// This fixes https://github.com/gardener/gardener/issues/7606
		deployGardenerResourceManager = g.Add(flow.Task{
			Name:         "Deploying gardener-resource-manager",
			Fn:           botanist.DeployGardenerResourceManager,
			SkipIf:       !cleanupShootResources || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(wakeUpKubeAPIServer),
		})
		ensureResourceManagerScaledUp = g.Add(flow.Task{
			Name:         "Ensuring that the gardener-resource-manager is scaled to 1",
			Fn:           botanist.ScaleGardenerResourceManagerToOne,
			SkipIf:       !cleanupShootResources || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deployGardenerResourceManager),
		})
		keepManagedResourcesObjectsInShoot = g.Add(flow.Task{
			Name:         "Configuring Managed Resources objects to be kept in the Shoot",
			Fn:           botanist.KeepObjectsForManagedResources,
			SkipIf:       !cleanupShootResources || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(ensureResourceManagerScaledUp),
		})
		deleteManagedResources = g.Add(flow.Task{
			Name:         "Deleting all Managed Resources from the Shoot's namespace",
			Fn:           botanist.DeleteManagedResources,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(keepManagedResourcesObjectsInShoot, ensureResourceManagerScaledUp),
		})
		waitForManagedResourcesDeletion = g.Add(flow.Task{
			Name:         "Waiting until ManagedResources are deleted",
			Fn:           flow.TaskFn(botanist.WaitUntilManagedResourcesDeleted).Timeout(10 * time.Minute),
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteManagedResources),
		})
		deleteMachineControllerManager = g.Add(flow.Task{
			Name: "Deleting machine-controller-manager",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.ControlPlane.MachineControllerManager.Destroy(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitForManagedResourcesDeletion),
		})
		waitUntilMachineControllerManagerDeleted = g.Add(flow.Task{
			Name: "Waiting until machine-controller-manager has been deleted",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.ControlPlane.MachineControllerManager.WaitCleanup(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteMachineControllerManager),
		})
		migrateExtensionResources = g.Add(flow.Task{
			Name:         "Migrating extension resources",
			Fn:           botanist.MigrateExtensionResourcesInParallel,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilMachineControllerManagerDeleted),
		})
		waitUntilExtensionResourcesMigrated = g.Add(flow.Task{
			Name:         "Waiting until extension resources have been migrated",
			Fn:           botanist.WaitUntilExtensionResourcesMigrated,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(migrateExtensionResources),
		})
		migrateExtensionsBeforeKubeAPIServer = g.Add(flow.Task{
			Name:         "Migrating extensions before kube-apiserver",
			Fn:           botanist.Shoot.Components.Extensions.Extension.MigrateBeforeKubeAPIServer,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitForManagedResourcesDeletion),
		})
		waitUntilExtensionsBeforeKubeAPIServerMigrated = g.Add(flow.Task{
			Name:         "Waiting until extensions that should be handled before kube-apiserver have been migrated",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitMigrateBeforeKubeAPIServer,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(migrateExtensionsBeforeKubeAPIServer),
		})
		persistShootStateAgain = g.Add(flow.Task{
			Name: "Persisting ShootState in garden cluster again",
			Fn: func(ctx context.Context) error {
				return shootstate.Deploy(ctx, r.Clock, botanist.GardenClient, botanist.SeedClientSet.Client(), botanist.Shoot.GetInfo(), false)
			},
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesMigrated),
		})
		deleteExtensionResources = g.Add(flow.Task{
			Name:         "Deleting extension resources from the Shoot namespace",
			Fn:           botanist.DestroyExtensionResourcesInParallel,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(persistShootStateAgain),
		})
		waitUntilExtensionResourcesDeleted = g.Add(flow.Task{
			Name:         "Waiting until extension resources have been deleted",
			Fn:           botanist.WaitUntilExtensionResourcesDeleted,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteExtensionResources),
		})
		deleteMachineResources = g.Add(flow.Task{
			Name:         "Shallow-deleting machine resources from the Shoot namespace",
			Fn:           botanist.ShallowDeleteMachineResources,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(persistShootStateAgain),
			// Dependencies: flow.NewTaskIDs(persistShootState),
		})
		waitUntilMachineResourcesDeleted = g.Add(flow.Task{
			Name: "Waiting until machine resources have been deleted",
			Fn: func(ctx context.Context) error {
				return gardenerutils.WaitUntilMachineResourcesDeleted(ctx, botanist.Logger, botanist.SeedClientSet.Client(), botanist.Shoot.SeedNamespace)
			},
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteMachineResources),
		})
		deleteExtensionsBeforeKubeAPIServer = g.Add(flow.Task{
			Name:         "Deleting extensions before kube-apiserver",
			Fn:           botanist.Shoot.Components.Extensions.Extension.DestroyBeforeKubeAPIServer,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesDeleted, waitUntilExtensionsBeforeKubeAPIServerMigrated),
		})
		waitUntilExtensionsBeforeKubeAPIServerDeleted = g.Add(flow.Task{
			Name:         "Waiting until extensions that should be handled before kube-apiserver have been deleted",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanupBeforeKubeAPIServer,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteExtensionsBeforeKubeAPIServer),
		})
		deleteStaleExtensionResources = g.Add(flow.Task{
			Name:         "Deleting stale extensions",
			Fn:           botanist.Shoot.Components.Extensions.Extension.DeleteStaleResources,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesMigrated),
		})
		waitUntilStaleExtensionResourcesDeleted = g.Add(flow.Task{
			Name:         "Waiting until all stale extensions have been deleted",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanupStaleResources,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteStaleExtensionResources),
		})
		migrateControlPlane = g.Add(flow.Task{
			Name: "Migrating shoot control plane",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.ControlPlane.Migrate(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionResourcesDeleted, waitUntilExtensionsBeforeKubeAPIServerDeleted, waitUntilStaleExtensionResourcesDeleted),
		})
		deleteControlPlane = g.Add(flow.Task{
			Name: "Deleting shoot control plane",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.ControlPlane.Destroy(ctx)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(migrateControlPlane),
		})
		waitUntilControlPlaneDeleted = g.Add(flow.Task{
			Name: "Waiting until shoot control plane has been deleted",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.ControlPlane.WaitCleanup(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteControlPlane),
		})
		waitUntilShootManagedResourcesDeleted = g.Add(flow.Task{
			Name:         "Waiting until shoot managed resources have been deleted",
			Fn:           flow.TaskFn(botanist.WaitUntilShootManagedResourcesDeleted).RetryUntilTimeout(defaultInterval, defaultTimeout),
			SkipIf:       !cleanupShootResources || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilControlPlaneDeleted),
		})

		// deleteKubeAPIServer = g.Add(flow.Task{
		// 	Name:         "Deleting kube-apiserver deployment",
		// 	Fn:           flow.TaskFn(botanist.DeleteKubeAPIServer).RetryUntilTimeout(defaultInterval, defaultTimeout),
		// 	Dependencies: flow.NewTaskIDs(waitForManagedResourcesDeletion, waitUntilEtcdReady, waitUntilControlPlaneDeleted, waitUntilShootManagedResourcesDeleted),
		// })
		// waitUntilKubeAPIServerDeleted = g.Add(flow.Task{
		// 	Name:         "Waiting until kube-apiserver has been deleted",
		// 	Fn:           botanist.Shoot.Components.ControlPlane.KubeAPIServer.WaitCleanup,
		// 	Dependencies: flow.NewTaskIDs(deleteKubeAPIServer),
		// })
		// migrateExtensionsAfterKubeAPIServer = g.Add(flow.Task{
		// 	Name:         "Migrating extensions after kube-apiserver",
		// 	Fn:           botanist.Shoot.Components.Extensions.Extension.MigrateAfterKubeAPIServer,
		// 	Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerDeleted),
		// })
		// waitUntilExtensionsAfterKubeAPIServerMigrated = g.Add(flow.Task{
		// 	Name:         "Waiting until extensions that should be handled after kube-apiserver have been migrated",
		// 	Fn:           botanist.Shoot.Components.Extensions.Extension.WaitMigrateAfterKubeAPIServer,
		// 	Dependencies: flow.NewTaskIDs(migrateExtensionsAfterKubeAPIServer),
		// })
		// deleteExtensionsAfterKubeAPIServer = g.Add(flow.Task{
		// 	Name:         "Deleting extensions after kube-apiserver",
		// 	Fn:           botanist.Shoot.Components.Extensions.Extension.DestroyAfterKubeAPIServer,
		// 	Dependencies: flow.NewTaskIDs(waitUntilExtensionsAfterKubeAPIServerMigrated),
		// })
		// waitUntilExtensionsAfterKubeAPIServerDeleted = g.Add(flow.Task{
		// 	Name:         "Waiting until extensions that should be handled after kube-apiserver have been deleted",
		// 	Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanupAfterKubeAPIServer,
		// 	Dependencies: flow.NewTaskIDs(deleteExtensionsAfterKubeAPIServer),
		// })
		// Add this step in interest of completeness. All extension deletions should have already been triggered by previous steps.
		// waitUntilExtensionsDeleted = g.Add(flow.Task{
		// 	Name:         "Waiting until all extensions have been deleted",
		// 	Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanup,
		// 	Dependencies: flow.NewTaskIDs(waitUntilExtensionsAfterKubeAPIServerMigrated),
		// })
		migrateInfrastructure = g.Add(flow.Task{
			Name: "Migrating shoot infrastructure",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.Infrastructure.Migrate(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilMachineResourcesDeleted, waitUntilShootManagedResourcesDeleted),
			// Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerDeleted),
		})
		waitUntilInfrastructureMigrated = g.Add(flow.Task{
			Name: "Waiting until shoot infrastructure has been migrated",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.Infrastructure.WaitMigrate(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(migrateInfrastructure),
		})
		deleteInfrastructure = g.Add(flow.Task{
			Name: "Deleting shoot infrastructure",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.Infrastructure.Destroy(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(waitUntilInfrastructureMigrated),
		})
		waitUntilInfrastructureDeleted = g.Add(flow.Task{
			Name: "Waiting until shoot infrastructure has been deleted",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return botanist.Shoot.Components.Extensions.Infrastructure.WaitCleanup(ctx)
			}),
			SkipIf:       o.Shoot.IsWorkerless || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(deleteInfrastructure),
		})
		// second stage is till here
		annotationSourceSecondStageIsReady = g.Add(flow.Task{
			Name: "Annotate shoot that second stage is ready",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := o.Shoot.UpdateInfo(ctx, o.GardenClient, false, func(shoot *gardencorev1beta1.Shoot) error {
					metav1.SetMetaDataAnnotation(&shoot.ObjectMeta, v1beta1constants.AnnotationSourceSecondStageIsReady, "true")
					return nil
				}); err != nil {
					return nil
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitUntilInfrastructureDeleted),
		})
		waitTargetThirdStageIsReady = g.Add(flow.Task{
			Name: "Waiting for the shoot.gardener.cloud/target-third-stage-ready annotation",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationTargetThirdStageIsReady)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(annotationSourceSecondStageIsReady),
		})
		migrateIngressDNSRecord = g.Add(flow.Task{
			Name:         "Migrating nginx ingress DNS record",
			Fn:           botanist.MigrateIngressDNSRecord,
			Dependencies: flow.NewTaskIDs(waitTargetThirdStageIsReady),
		})
		migrateExternalDNSRecord = g.Add(flow.Task{
			Name:         "Migrating external domain DNS record",
			Fn:           botanist.MigrateExternalDNSRecord,
			Dependencies: flow.NewTaskIDs(waitTargetThirdStageIsReady),
		})
		migrateInternalDNSRecord = g.Add(flow.Task{
			Name:         "Migrating internal domain DNS record",
			Fn:           botanist.MigrateInternalDNSRecord,
			Dependencies: flow.NewTaskIDs(waitTargetThirdStageIsReady),
		})
		syncPoint = flow.NewTaskIDs(
			// waitUntilExtensionsAfterKubeAPIServerDeleted,
			waitUntilMachineResourcesDeleted,
			// waitUntilExtensionsDeleted,
			waitUntilInfrastructureDeleted,
		)

		// dns migrated
		annotationDNSMigrated = g.Add(flow.Task{
			Name: "Annotate dns has been migrated",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := o.Shoot.UpdateInfo(ctx, o.GardenClient, false, func(shoot *gardencorev1beta1.Shoot) error {
					metav1.SetMetaDataAnnotation(&shoot.ObjectMeta, v1beta1constants.AnnotationDNSMigrated, "true")
					return nil
				}); err != nil {
					return nil
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitTargetThirdStageIsReady, migrateIngressDNSRecord, migrateExternalDNSRecord, migrateInternalDNSRecord, syncPoint),
		})
		waitTargetDNSRestored = g.Add(flow.Task{
			Name: "Waiting for the shoot.gardener.cloud/dns-restored annotation",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationDNSRestored)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(annotationDNSMigrated),
		})
		waitTargetFourthStageIsReady = g.Add(flow.Task{
			Name: "Waiting for the shoot.gardener.cloud/target-fourth-stage-ready annotation",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationTargetFourthStageIsReady)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitTargetDNSRestored),
		})
		destroyDNSRecords = g.Add(flow.Task{
			Name:         "Deleting DNSRecords from the Shoot namespace",
			Fn:           botanist.DestroyDNSRecords,
			SkipIf:       !nonTerminatingNamespace,
			Dependencies: flow.NewTaskIDs(waitTargetDNSRestored, waitTargetFourthStageIsReady),
		})
		deleteKubeAPIServer = g.Add(flow.Task{
			Name:         "Deleting kube-apiserver deployment",
			Fn:           flow.TaskFn(botanist.DeleteKubeAPIServer).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitTargetFourthStageIsReady, destroyDNSRecords),
		})
		waitUntilKubeAPIServerDeleted = g.Add(flow.Task{
			Name:         "Waiting until kube-apiserver has been deleted",
			Fn:           botanist.Shoot.Components.ControlPlane.KubeAPIServer.WaitCleanup,
			Dependencies: flow.NewTaskIDs(deleteKubeAPIServer),
		})
		migrateExtensionsAfterKubeAPIServer = g.Add(flow.Task{
			Name:         "Migrating extensions after kube-apiserver",
			Fn:           botanist.Shoot.Components.Extensions.Extension.MigrateAfterKubeAPIServer,
			Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerDeleted),
		})
		waitUntilExtensionsAfterKubeAPIServerMigrated = g.Add(flow.Task{
			Name:         "Waiting until extensions that should be handled after kube-apiserver have been migrated",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitMigrateAfterKubeAPIServer,
			Dependencies: flow.NewTaskIDs(migrateExtensionsAfterKubeAPIServer),
		})
		deleteExtensionsAfterKubeAPIServer = g.Add(flow.Task{
			Name:         "Deleting extensions after kube-apiserver",
			Fn:           botanist.Shoot.Components.Extensions.Extension.DestroyAfterKubeAPIServer,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionsAfterKubeAPIServerMigrated),
		})
		waitUntilExtensionsAfterKubeAPIServerDeleted = g.Add(flow.Task{
			Name:         "Waiting until extensions that should be handled after kube-apiserver have been deleted",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanupAfterKubeAPIServer,
			Dependencies: flow.NewTaskIDs(deleteExtensionsAfterKubeAPIServer),
		})
		// Add this step in interest of completeness. All extension deletions should have already been triggered by previous steps.
		waitUntilExtensionsDeleted = g.Add(flow.Task{
			Name:         "Waiting until all extensions have been deleted",
			Fn:           botanist.Shoot.Components.Extensions.Extension.WaitCleanup,
			Dependencies: flow.NewTaskIDs(waitUntilExtensionsAfterKubeAPIServerMigrated),
		})
		syncPointKapiDeleted = flow.NewTaskIDs(
			waitUntilExtensionsAfterKubeAPIServerDeleted,
			waitUntilExtensionsDeleted,
		)
		// third stage is till here
		annotationSourceThirdStageIsReady = g.Add(flow.Task{
			Name: "Annotate shoot that third stage is ready",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				if err := o.Shoot.UpdateInfo(ctx, o.GardenClient, false, func(shoot *gardencorev1beta1.Shoot) error {
					metav1.SetMetaDataAnnotation(&shoot.ObjectMeta, v1beta1constants.AnnotationSourceThirdStageIsReady, "true")
					return nil
				}); err != nil {
					return nil
				}
				return nil
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(syncPointKapiDeleted),
		})
		createETCDSnapshot = g.Add(flow.Task{
			Name:         "Creating ETCD Snapshot",
			Fn:           botanist.SnapshotEtcd,
			SkipIf:       !etcdSnapshotRequired || etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(syncPoint, waitUntilKubeAPIServerDeleted),
		})
		migrateBackupEntryInGarden = g.Add(flow.Task{
			Name:         "Migrating BackupEntry to new seed",
			Fn:           botanist.Shoot.Components.BackupEntry.Migrate,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(syncPoint, createETCDSnapshot),
		})
		waitUntilBackupEntryInGardenMigrated = g.Add(flow.Task{
			Name:         "Waiting for BackupEntry to be migrated to new seed",
			Fn:           botanist.Shoot.Components.BackupEntry.WaitMigrate,
			SkipIf:       etcdMemberRemoved,
			Dependencies: flow.NewTaskIDs(migrateBackupEntryInGarden),
		})
		// destroyEtcd = g.Add(flow.Task{
		// 	Name:         "Destroying main and events etcd",
		// 	Fn:           flow.TaskFn(botanist.DestroyEtcd).RetryUntilTimeout(defaultInterval, defaultTimeout),
		// 	Dependencies: flow.NewTaskIDs(syncPoint, createETCDSnapshot, waitUntilBackupEntryInGardenMigrated),
		// })
		// waitUntilEtcdDeleted = g.Add(flow.Task{
		// 	Name:         "Waiting until main and event etcd have been destroyed",
		// 	Fn:           flow.TaskFn(botanist.WaitUntilEtcdsDeleted).RetryUntilTimeout(defaultInterval, defaultTimeout),
		// 	Dependencies: flow.NewTaskIDs(destroyEtcd),
		// })
		// deleteNamespace = g.Add(flow.Task{
		// 	Name:         "Deleting shoot namespace in Seed",
		// 	Fn:           flow.TaskFn(botanist.DeleteSeedNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
		// 	Dependencies: flow.NewTaskIDs(syncPoint, waitUntilBackupEntryInGardenMigrated, deleteExtensionResources, destroyDNSRecords, waitForManagedResourcesDeletion, waitUntilEtcdDeleted),
		// })
		// _ = g.Add(flow.Task{
		// 	Name:         "Waiting until shoot namespace in Seed has been deleted",
		// 	Fn:           botanist.WaitUntilSeedNamespaceDeleted,
		// 	Dependencies: flow.NewTaskIDs(deleteNamespace),
		// })

		etcdMemberRemovedTask = g.Add(flow.Task{
			Name: "wait for annotation etcd member removal",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return o.WaitForShootAnnotation(ctx, v1beta1constants.AnnotationEtcdMemberRemoved)
			}).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(annotationSourceThirdStageIsReady, waitUntilBackupEntryInGardenMigrated),
		})

		destroyEtcd = g.Add(flow.Task{
			Name:         "Destroying main and events etcd",
			Fn:           flow.TaskFn(botanist.DestroyEtcd).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(syncPoint, etcdMemberRemovedTask),
			// Dependencies: flow.NewTaskIDs(syncPoint, createETCDSnapshot, waitUntilBackupEntryInGardenMigrated),
		})
		waitUntilEtcdDeleted = g.Add(flow.Task{
			Name:         "Waiting until main and event etcd have been destroyed",
			Fn:           flow.TaskFn(botanist.WaitUntilEtcdsDeleted).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(destroyEtcd),
		})
		deleteNamespace = g.Add(flow.Task{
			Name: "Deleting shoot namespace in Seed",
			Fn:   flow.TaskFn(botanist.DeleteSeedNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
			// Dependencies: flow.NewTaskIDs(syncPoint, waitUntilBackupEntryInGardenMigrated, deleteExtensionResources, destroyDNSRecords, waitForManagedResourcesDeletion, waitUntilEtcdDeleted),
			Dependencies: flow.NewTaskIDs(waitUntilEtcdDeleted),
		})
		_ = g.Add(flow.Task{
			Name:         "Waiting until shoot namespace in Seed has been deleted",
			Fn:           botanist.WaitUntilSeedNamespaceDeleted,
			Dependencies: flow.NewTaskIDs(deleteNamespace),
		})

		f = g.Compile()
	)

	if err := f.Run(ctx, flow.Opts{
		Log:              o.Logger,
		ProgressReporter: r.newProgressReporter(o.ReportShootProgress),
		ErrorContext:     errorContext,
		ErrorCleaner:     o.CleanShootTaskError,
	}); err != nil {
		return v1beta1helper.NewWrappedLastErrors(v1beta1helper.FormatLastErrDescription(err), flow.Errors(err))
	}

	o.Logger.Info("Successfully prepared Shoot cluster for restoration")
	return nil
}