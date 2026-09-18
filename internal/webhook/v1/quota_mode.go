/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// gpuRequestsResource is the ResourceQuota key that caps how many devices a namespace may request.
//
// Duplicated from internal/controller for the reason kueueQueueLabel is: a webhook importing the controller
// package to read one string would drag the whole reconciler into the admission binary's dependency graph.
const gpuRequestsResource = corev1.ResourceName("requests.nvidia.com/gpu")

// quotaName is how GPUQuotaPolicyReconciler names the ResourceQuota it syncs.
//
// Duplicated for the same reason, and it must not drift: a guard that looks for a differently named object
// finds nothing and concludes there is no cap, which is the reading that refuses legitimate work.
// TestTheQuotaNameMatchesTheController holds the two together.
func quotaName(policyName string) string { return "gpuquota-" + policyName }

// meteredByResourceQuota reports whether this namespace's GPU budget is held by a ResourceQuota rather than
// by a Kueue ClusterQueue, and that the cap actually exists.
//
// The queue-name label is what makes a workload visible to Kueue's accounting, so demanding it is right
// exactly where Kueue is the authority. Where a GPUQuotaPolicy leaves trainingQuota off, the controller does
// the opposite of what the Pod guard's header describes: it KEEPS the namespace ResourceQuota capping
// requests.nvidia.com/gpu (internal/controller/gpuquotapolicy_controller.go, syncNamespaceResourceQuota) and
// creates no queues at all (kueue_quota.go deletes them). A Job in such a namespace is already metered, and
// refusing it for carrying no queue label refused work the platform had agreed to count — with no queue in
// existence for the tenant to name.
//
// Three things make this answer conservative rather than convenient:
//
//   - A single policy with trainingQuota set anywhere over this namespace returns false. Two policies may
//     target one namespace (they cap the same key and Kubernetes AND-s them), so a mixed namespace is one
//     where Kueue is an authority for something, and the queue requirement stays.
//   - The ResourceQuota must be readable AND carry the GPU key. A policy that says false while its quota is
//     missing, still being created, or capping something else is not a metered namespace; it is a namespace
//     with no ceiling, which is what the guard exists to refuse.
//   - Every read error is returned, never swallowed. The callers fail closed on it, for the reason the
//     webhook's failurePolicy is Fail: a guard that stops applying when a read fails admits exactly what it
//     exists to refuse, invisibly.
//
// It does NOT widen anything else. A Pod that never passed the requester check does not reach this, so bare
// Pods and serving Pods are refused exactly as before.
func meteredByResourceQuota(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	if reader == nil {
		return false, fmt.Errorf("no reader wired to consult %q's quota mode", namespace)
	}

	var policies platformv1.GPUQuotaPolicyList
	if err := reader.List(ctx, &policies); err != nil {
		return false, fmt.Errorf("list GPUQuotaPolicy while deciding %q's quota mode: %w", namespace, err)
	}

	var resourceQuotaOnly []platformv1.GPUQuotaPolicy
	for i := range policies.Items {
		p := policies.Items[i]
		if p.Spec.TargetNamespace != namespace {
			continue
		}
		if p.Spec.TrainingQuota {
			// Kueue holds the budget for at least part of this namespace. Keep the queue requirement.
			return false, nil
		}
		resourceQuotaOnly = append(resourceQuotaOnly, p)
	}
	if len(resourceQuotaOnly) == 0 {
		// No policy governs this namespace through a ResourceQuota. Nothing here says the work is metered.
		return false, nil
	}

	for i := range resourceQuotaOnly {
		p := resourceQuotaOnly[i]
		var rq corev1.ResourceQuota
		key := client.ObjectKey{Name: quotaName(p.Name), Namespace: namespace}
		switch err := reader.Get(ctx, key, &rq); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return false, fmt.Errorf("read ResourceQuota %s while deciding %q's quota mode: %w", key, namespace, err)
		}
		if _, capped := rq.Spec.Hard[gpuRequestsResource]; capped {
			return true, nil
		}
	}
	return false, nil
}
