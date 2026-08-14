// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	corebackupentry "github.com/gardener/gardener/pkg/component/garden/backupentry"
)

// DefaultCoreBackupEntry creates the default deployer for the core.gardener.cloud/v1beta1.BackupEntry resource.
func (b *Botanist) DefaultCoreBackupEntry() corebackupentry.Interface {
	ownerRef := metav1.NewControllerRef(b.Shoot.GetInfo(), gardencorev1beta1.SchemeGroupVersion.WithKind("Shoot"))
	ownerRef.BlockOwnerDeletion = new(false)

	values := &corebackupentry.Values{
		Namespace:      b.Shoot.GetInfo().Namespace,
		Name:           b.Shoot.BackupEntryName,
		ShootPurpose:   b.Shoot.GetInfo().Spec.Purpose,
		OwnerReference: ownerRef,
		SeedName:       b.Shoot.GetInfo().Spec.SeedName,
		BucketName:     string(b.Shoot.GetInfo().Status.UID),
	}

	if !b.Shoot.IsSelfHosted() {
		values.BucketName = string(b.Seed.GetInfo().UID)
	}

	if b.Shoot.IsSelfHosted() {
		values.SeedName = nil
		values.Shoot = b.Shoot.GetInfo()
	}

	return corebackupentry.New(
		b.Logger,
		b.GardenClient,
		values,
		corebackupentry.DefaultInterval,
		corebackupentry.DefaultTimeout,
	)
}

// DeployBackupEntry deploys the BackupEntry resource in the Garden cluster and triggers the restore operation in case
// the Shoot is in the restore phase of the control plane migration.
func (b *Botanist) DeployBackupEntry(ctx context.Context) error {
	if b.Shoot.IsRestorePhase() {
		return b.Shoot.Components.BackupEntry.Restore(ctx, b.Shoot.GetShootState())
	}
	return b.Shoot.Components.BackupEntry.Deploy(ctx)
}

// SourceBackupEntry creates a deployer for a core.gardener.cloud/v1beta1.BackupEntry resource which will be used
// as source when copying etcd backups.
func (b *Botanist) SourceBackupEntry() corebackupentry.Interface {
	ownerRef := metav1.NewControllerRef(b.Shoot.GetInfo(), gardencorev1beta1.SchemeGroupVersion.WithKind("Shoot"))
	ownerRef.BlockOwnerDeletion = new(false)

	return corebackupentry.New(
		b.Logger,
		b.GardenClient,
		&corebackupentry.Values{
			Namespace:      b.Shoot.GetInfo().Namespace,
			Name:           fmt.Sprintf("%s-%s", v1beta1constants.BackupSourcePrefix, b.Shoot.BackupEntryName),
			ShootPurpose:   b.Shoot.GetInfo().Spec.Purpose,
			OwnerReference: ownerRef,
			SeedName:       b.Shoot.GetInfo().Spec.SeedName,
		},
		corebackupentry.DefaultInterval,
		corebackupentry.DefaultTimeout,
	)
}

// DeploySourceBackupEntry deploys the source BackupEntry and sets its bucketName to be equal to the bucketName of the shoot's original
// BackupEntry if the source BackupEntry doesn't already exist.
func (b *Botanist) DeploySourceBackupEntry(ctx context.Context) error {
	bucketName := b.Shoot.Components.BackupEntry.GetActualBucketName()
	if _, err := b.Shoot.Components.SourceBackupEntry.Get(ctx); err == nil {
		bucketName = b.Shoot.Components.SourceBackupEntry.GetActualBucketName()
	} else if client.IgnoreNotFound(err) != nil {
		return err
	}

	b.Shoot.Components.SourceBackupEntry.SetBucketName(bucketName)
	return b.Shoot.Components.SourceBackupEntry.Deploy(ctx)
}

// DestroySourceBackupEntry destroys the source BackupEntry.
func (b *Botanist) DestroySourceBackupEntry(ctx context.Context) error {
	if err := b.Shoot.Components.SourceBackupEntry.SetForceDeletionAnnotation(ctx); err != nil {
		return err
	}

	return b.Shoot.Components.SourceBackupEntry.Destroy(ctx)
}

// DeployLiveMigrationBackupEntry deploys the backup entry for the destination seed during a live control plane
// migration. Unlike a normal control plane migration, live migration does not copy etcd backups — data is
// replicated via the joint etcd cluster. To avoid triggering the source→destination backup-copy path (which
// fires when status.seedName != spec.seedName), this function clears the stale status.seedName first so the
// gardenlet reconciles the entry as a fresh creation on the destination.
func (b *Botanist) DeployLiveMigrationBackupEntry(ctx context.Context) error {
	be := &gardencorev1beta1.BackupEntry{}
	if err := b.GardenClient.Get(ctx, client.ObjectKey{
		Namespace: b.Shoot.GetInfo().Namespace,
		Name:      b.Shoot.BackupEntryName,
	}, be); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get backup entry: %w", err)
		}
		// Not found — will be created fresh by Deploy().
	} else if be.Status.SeedName != nil && *be.Status.SeedName != ptr.Deref(b.Shoot.GetInfo().Spec.SeedName, "") {
		// The existing entry was reconciled on the source seed. Clear status.seedName so it is treated as a
		// fresh creation on the destination, bypassing the source→destination backup-copy migration path.
		patch := client.MergeFrom(be.DeepCopy())
		be.Status.SeedName = nil
		if err := b.GardenClient.Status().Patch(ctx, be, patch); err != nil {
			return fmt.Errorf("failed to clear backup entry status seed name: %w", err)
		}
	}
	return b.Shoot.Components.BackupEntry.Deploy(ctx)
}
