// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener/pkg/apis/core"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
)

// LoadConfig contains the configuration for load testing operations.
type LoadConfig struct {
	// Resources contains the configuration for each resource type to be operated on.
	Resources []OperationLoads `json:"resources"`
	// GetShootPeriod defines the period to perform get operations on shoots.
	GetShootPeriod time.Duration `json:"getShootPeriod"`
}

type OperationLoads struct {
	Group   string  `json:"group"`   // API group of the resource
	Version string  `json:"version"` // API version of the resource
	Kind    string  `json:"kind"`    // Kind of the resource
	Get     float64 `json:"get"`
	Patch   float64 `json:"patch"`
	Post    float64 `json:"post"`
	List    float64 `json:"list"`
}

// loadTest implements manager.Runnable and performs dummy operations what a ususal seed would do.
type loadTest struct {
	// gardenClient is the garden cluster client.
	gardenClient client.Client
	// log is a logger.
	log logr.Logger
	// seedName is the name of the seed.
	seedName string
	// loadConfig contains the load configuration.
	loadConfig LoadConfig
}

func (l loadTest) Start(ctx context.Context) error {
	if l.gardenClient == nil {
		return fmt.Errorf("garden client is required")
	}

	if l.seedName == "" {
		return fmt.Errorf("seed name is required")
	}

	l.log.Info("Starting dummy seed operations", "seedName", l.seedName)

	// wait for seed to be there first for the first time
	for {
		seed := &gardencorev1beta1.Seed{ObjectMeta: metav1.ObjectMeta{Name: l.seedName}}
		if err := l.gardenClient.Get(ctx, client.ObjectKeyFromObject(seed), seed); err != nil {
			if apierrors.IsNotFound(err) {
				l.log.Info("Seed not found yet, waiting...", "seed", l.seedName)
			} else {
				l.log.Info("Failed to get Seed, retrying...", "error", err)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		} else {
			l.log.Info("Seed found, starting operations", "seed", l.seedName)
			break
		}
	}

	var (
		opsCancel        context.CancelFunc
		opsCtx           context.Context
		lastShootsCounts = 0
	)

	for {
		shootCount, err := l.listSeedShoots(ctx)
		if err != nil {
			l.log.Info("Failed to list shoots, retrying in 1 minute", "error", err)
		} else {
			l.log.Info("Listed shoots successfully", "shootCount", shootCount)
			if shootCount != lastShootsCounts {
				lastShootsCounts = shootCount

				// Cancel the previous operations context if it exists
				if opsCancel != nil {
					l.log.Info("Cancelling previous operations", "seedName", l.seedName)
					opsCancel()
					// Give some time for goroutines to stop
					time.Sleep(100 * time.Millisecond)
				}

				opsCtx, opsCancel = context.WithCancel(ctx)

				for _, r := range l.loadConfig.Resources {
					resource := r
					l.log.Info("Starting dummy operations for resource", "group", resource.Group, "version", resource.Version, "kind", resource.Kind)
					getRequests, getRepeatTime := requestCount(shootCount, resource.Get)
					patchRequests, patchRepeatTime := requestCount(shootCount, resource.Patch)
					updateRequests, updateRepeatTime := requestCount(shootCount, resource.Post)
					listRequests, listRepeatTime := requestCount(shootCount, resource.List)

					objectList, err := l.listObjectsForResourceKind(ctx, resource.Kind)
					if err != nil {
						l.log.Error(err, "Failed to list objects for dummy operations", "kind", resource.Kind)
						continue
					}

					if getRepeatTime > 0 {
						go func(res OperationLoads, objs []client.Object, reqs int, repeatTime int) {
							// Dummy Get operations
							for {
								select {
								case <-opsCtx.Done():
									l.log.Info("Stopping Get operations for resource", "kind", res.Kind)
									return
								default:
									startTime := time.Now()
									l.performGetRequests(opsCtx, res, objs, reqs)
									endTime := time.Now()
									diff := endTime.Sub(startTime)
									if time.Duration(repeatTime)*time.Second > diff {
										sleepTime := time.Duration(repeatTime)*time.Second - diff
										select {
										case <-opsCtx.Done():
											return
										case <-time.After(sleepTime):
										}
									}
								}
							}
						}(resource, objectList, getRequests, getRepeatTime)
					}

					if patchRepeatTime > 0 {
						go func(res OperationLoads, reqs int, repeatTime int) {
							// Dummy Patch operations
							for {
								select {
								case <-opsCtx.Done():
									l.log.Info("Stopping Patch operations for resource", "kind", res.Kind)
									return
								default:
									startTime := time.Now()
									l.performPatchRequests(opsCtx, res, reqs)
									endTime := time.Now()
									diff := endTime.Sub(startTime)
									if time.Duration(repeatTime)*time.Second > diff {
										sleepTime := time.Duration(repeatTime)*time.Second - diff
										select {
										case <-opsCtx.Done():
											return
										case <-time.After(sleepTime):
										}
									}
								}
							}
						}(resource, patchRequests, patchRepeatTime)
					}

					if updateRepeatTime > 0 {
						go func(res OperationLoads, reqs int, repeatTime int) {
							// Dummy Update operations
							for {
								select {
								case <-opsCtx.Done():
									l.log.Info("Stopping Update operations for resource", "kind", res.Kind)
									return
								default:
									startTime := time.Now()
									l.performUpdateRequests(opsCtx, res, reqs)
									endTime := time.Now()
									diff := endTime.Sub(startTime)
									if time.Duration(repeatTime)*time.Second > diff {
										sleepTime := time.Duration(repeatTime)*time.Second - diff
										select {
										case <-opsCtx.Done():
											return
										case <-time.After(sleepTime):
										}
									}
								}
							}
						}(resource, updateRequests, updateRepeatTime)
					}

					if listRepeatTime > 0 {
						go func(res OperationLoads, reqs int, repeatTime int) {
							// Dummy List operations
							for {
								select {
								case <-opsCtx.Done():
									l.log.Info("Stopping List operations for resource", "kind", res.Kind)
									return
								default:
									startTime := time.Now()
									l.performListRequests(opsCtx, res, reqs)
									endTime := time.Now()
									diff := endTime.Sub(startTime)
									if time.Duration(repeatTime)*time.Second > diff {
										sleepTime := time.Duration(repeatTime)*time.Second - diff
										select {
										case <-opsCtx.Done():
											return
										case <-time.After(sleepTime):
										}
									}
								}
							}
						}(resource, listRequests, listRepeatTime)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			// Cancel any running operations before exiting
			if opsCancel != nil {
				opsCancel()
			}
			return ctx.Err()
		case <-time.After(l.loadConfig.GetShootPeriod):
			// Continue loop
		}
	}
}

func (l loadTest) performGetRequests(ctx context.Context, resource OperationLoads, objectList []client.Object, requests int) {
	if len(objectList) == 0 {
		l.log.Info("No objects available for Get operation", "kind", resource.Kind)
		return
	}

	count := min(requests, len(objectList))

	for i := range count {
		select {
		case <-ctx.Done():
			return
		default:
		}

		obj := objectList[i]
		key := client.ObjectKeyFromObject(obj)

		go func(o client.Object, k client.ObjectKey) {
			newObj := o.DeepCopyObject().(client.Object)
			if err := l.gardenClient.Get(ctx, k, newObj); err != nil {
				l.log.Error(err, "Failed to perform Get operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			} else {
				l.log.Info("Successfully performed Get operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			}
		}(obj, key)
	}
}

func (l loadTest) performPatchRequests(ctx context.Context, resource OperationLoads, requests int) {
	objectList, err := l.listObjectsForResourceKind(ctx, resource.Kind)
	if err != nil {
		l.log.Error(err, "Failed to list objects for Patch operation", "kind", resource.Kind)
		return
	}

	if len(objectList) == 0 {
		l.log.Info("No objects available for Patch operation", "kind", resource.Kind)
		return
	}

	count := min(requests, len(objectList))

	for i := range count {
		select {
		case <-ctx.Done():
			return
		default:
		}

		obj := objectList[i]
		key := client.ObjectKeyFromObject(obj)

		go func(o client.Object, k client.ObjectKey) {
			// Create a copy for patching
			original := o.DeepCopyObject().(client.Object)
			patched := o.DeepCopyObject().(client.Object)

			// Add a dummy annotation for the patch
			annotations := patched.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations["gardenlet.gardener.cloud/dummy-patch-timestamp"] = time.Now().Format(time.RFC3339)
			patched.SetAnnotations(annotations)

			// Perform the patch
			if err := l.gardenClient.Patch(ctx, patched, client.MergeFrom(original)); err != nil {
				l.log.Error(err, "Failed to perform Patch operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			} else {
				l.log.Info("Successfully performed Patch operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			}
		}(obj, key)
	}
}

func (l loadTest) performUpdateRequests(ctx context.Context, resource OperationLoads, requests int) {
	objectList, err := l.listObjectsForResourceKind(ctx, resource.Kind)
	if err != nil {
		l.log.Error(err, "Failed to list objects for Update operation", "kind", resource.Kind)
		return
	}

	if len(objectList) == 0 {
		l.log.Info("No objects available for Update operation", "kind", resource.Kind)
		return
	}

	count := min(requests, len(objectList))

	for i := range count {
		select {
		case <-ctx.Done():
			return
		default:
		}

		obj := objectList[i]
		key := client.ObjectKeyFromObject(obj)

		go func(o client.Object, k client.ObjectKey) {
			// Create a copy for updating
			updated := o.DeepCopyObject().(client.Object)

			// Add a dummy annotation for the update
			annotations := updated.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations["gardenlet.gardener.cloud/dummy-update-timestamp"] = time.Now().Format(time.RFC3339)
			updated.SetAnnotations(annotations)

			// Perform the update
			if err := l.gardenClient.Update(ctx, updated); err != nil {
				l.log.Error(err, "Failed to perform Update operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			} else {
				l.log.Info("Successfully performed Update operation", "kind", resource.Kind, "name", k.Name, "namespace", k.Namespace)
			}
		}(obj, key)
	}
}

func (l loadTest) performListRequests(ctx context.Context, resource OperationLoads, requests int) {
	for range requests {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if _, err := l.listObjectsForResourceKind(ctx, resource.Kind); err != nil {
			l.log.Error(err, "Failed to perform List operation", "kind", resource.Kind)
		} else {
			l.log.Info("Successfully performed List operation", "kind", resource.Kind)
		}
	}
}

func (l loadTest) listObjectsForResourceKind(ctx context.Context, kind string) ([]client.Object, error) {
	var objectList []client.Object

	switch kind {
	case "Shoot":
		shootList := &gardencorev1beta1.ShootList{}
		if err := l.gardenClient.List(ctx, shootList, client.MatchingFields{core.ShootSeedName: l.seedName}); err != nil {
			return nil, err
		}

		for i := range shootList.Items {
			objectList = append(objectList, &shootList.Items[i])
		}
	case "Seed":
		seedList := &gardencorev1beta1.SeedList{}
		if err := l.gardenClient.List(ctx, seedList); err != nil {
			return nil, err
		}

		for i := range seedList.Items {
			objectList = append(objectList, &seedList.Items[i])
		}
	case "ManagedSeed":
		managedSeedList := &seedmanagementv1alpha1.ManagedSeedList{}
		if err := l.gardenClient.List(ctx, managedSeedList); err != nil {
			return nil, err
		}

		for i := range managedSeedList.Items {
			objectList = append(objectList, &managedSeedList.Items[i])
		}
	case "ConfigMap":
		seedNamespace := gardenerutils.ComputeGardenNamespace(l.seedName)
		configMapList := &corev1.ConfigMapList{}
		if err := l.gardenClient.List(ctx, configMapList, client.InNamespace(seedNamespace)); err != nil {
			return nil, err
		}

		for i := range configMapList.Items {
			objectList = append(objectList, &configMapList.Items[i])
		}
	case "Secret":
		seedNamespace := gardenerutils.ComputeGardenNamespace(l.seedName)
		secretList := &corev1.SecretList{}
		if err := l.gardenClient.List(ctx, secretList, client.InNamespace(seedNamespace)); err != nil {
			return nil, err
		}

		for i := range secretList.Items {
			objectList = append(objectList, &secretList.Items[i])
		}
	case "ServiceAccount":
		seedNamespace := gardenerutils.ComputeGardenNamespace(l.seedName)
		serviceAccountList := &corev1.ServiceAccountList{}
		if err := l.gardenClient.List(ctx, serviceAccountList, client.InNamespace(seedNamespace)); err != nil {
			return nil, err
		}

		for i := range serviceAccountList.Items {
			objectList = append(objectList, &serviceAccountList.Items[i])
		}
	default:
		return nil, fmt.Errorf("unsupported resource kind: %s", kind)
	}

	return objectList, nil
}

// requestCount calculates the number of requests and the interval in seconds between each request
func requestCount(shootCount int, requestPerSecond float64) (int, int) {
	totalRequests := requestPerSecond * float64(shootCount)

	if totalRequests < 1 && totalRequests > 0 {
		return 1, int(1 / totalRequests)

	} else if totalRequests >= 1 {
		return int(totalRequests), 1
	}

	return 0, 0
}

// listSeedShoots lists all shoots scheduled on the seed.
func (l loadTest) listSeedShoots(ctx context.Context) (int, error) {
	shootList := &gardencorev1beta1.ShootList{}
	if err := l.gardenClient.List(ctx, shootList, client.MatchingFields{core.ShootSeedName: l.seedName}); err != nil {
		return 0, err
	}

	l.log.Info("Successfully listed shoots for seed", "seedName", l.seedName, "shootCount", len(shootList.Items))

	return len(shootList.Items), nil
}
