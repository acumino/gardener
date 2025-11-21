// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"slices"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
)

// ValidateZone validates the provided zone against the zones configured for the given worker pool.
func ValidateZone(worker gardencorev1beta1.Worker, providedZone string) (string, error) {
	workerZones := worker.Zones

	if providedZone == "" {
		if len(workerZones) == 0 {
			return "", nil
		}
		if len(workerZones) == 1 {
			return workerZones[0], nil
		}
		return "", fmt.Errorf("worker %q has multiple zones configured %v, --zone flag is required", worker.Name, workerZones)
	}

	if providedZone != "" {
		if !slices.Contains(workerZones, providedZone) {
			return "", fmt.Errorf("provided zone %q is not one of the configured zones %v for worker %q", providedZone, workerZones, worker.Name)
		}
	}

	return providedZone, nil
}
