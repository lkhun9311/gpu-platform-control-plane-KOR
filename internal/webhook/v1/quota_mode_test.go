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
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// The defect these tests pin: a tenant that never asked for Kueue could not submit a GPU Job at all.
//
// GPUQuotaPolicyReconciler labels the namespace gpu-quota-enforced whatever the policy says, and both guards
// select on that label. With trainingQuota off it then deletes the queues and keeps the namespace
// ResourceQuota capping requests.nvidia.com/gpu. So the Job guard demanded a queue label in a namespace
// where no queue exists, the Pod guard refused the Pod for the same reason, and the policy reported Synced
// throughout. Nothing was over-admitted; ordinary metered work was refused with no route open to the tenant.

// policy returns a GPUQuotaPolicy targeting ns, in whichever mode.
func policy(name, ns string, training bool) *platformv1.GPUQuotaPolicy {
	return &platformv1.GPUQuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: platformv1.GPUQuotaPolicySpec{
			TargetNamespace: ns,
			TrainingQuota:   training,
		},
	}
}

// gpuQuotaFor builds the ResourceQuota the controller syncs for a ResourceQuota-mode policy.
func gpuQuotaFor(policyName, ns string, capped bool) *corev1.ResourceQuota {
	hard := corev1.ResourceList{}
	if capped {
		hard[gpuRequestsResource] = *resource.NewQuantity(4, resource.DecimalSI)
	} else {
		// A quota that exists but caps something else is not a GPU ceiling.
		hard[corev1.ResourceName("pods")] = *resource.NewQuantity(10, resource.DecimalSI)
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: quotaName(policyName), Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}
}

// unqueuedJob is a GPU Job with no queue label, which is all a ResourceQuota-mode tenant can write.
func unqueuedJob() *batchv1.Job {
	return gpuJob(2, func(j *batchv1.Job) { j.Labels = nil })
}

// Mutation that turns this red: delete the meteredByResourceQuota branch from the Job guard, or make it
// return false unconditionally.
func TestAnUnqueuedGPUJobIsAdmittedWhereAResourceQuotaMetersIt(t *testing.T) {
	res := askJob(t, unqueuedJob(),
		policy("team-a-quota", "team-a", false),
		gpuQuotaFor("team-a-quota", "team-a", true))
	if !res.Allowed {
		t.Fatalf("refused the only shape a ResourceQuota-mode tenant can submit: %s", res.Result.Message)
	}
}

// The Pod half. Refusing here would move the refusal rather than remove it: the Job controller creates the
// Pod, and the Pod carries no queue label because the Job carries none either.
//
// Mutation that turns this red: delete the meteredByResourceQuota branch from the Pod guard.
func TestAPodFromAnUnqueuedJobIsAdmittedWhereAResourceQuotaMetersIt(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "train-1", Namespace: "team-a"}}
	v := gpuValidator(t, job,
		policy("team-a-quota", "team-a", false),
		gpuQuotaFor("team-a-quota", "team-a", true))
	res := ask(t, v, jobControllerUser, gpuPod(1, ownedBy("train-1")))
	if !res.Allowed {
		t.Fatalf("refused a Pod the namespace ResourceQuota already counts: %s", res.Result.Message)
	}
}

// One policy with trainingQuota set makes Kueue an authority over this namespace, and two policies may
// target one namespace. Admitting on the first false one found would hand a Kueue-governed namespace an
// unmetered path.
//
// Mutation that turns this red: return true on the first ResourceQuota-mode policy instead of refusing the
// whole namespace when any policy sets trainingQuota.
func TestAMixedNamespaceKeepsTheQueueRequirement(t *testing.T) {
	res := askJob(t, unqueuedJob(),
		policy("team-a-quota", "team-a", false),
		gpuQuotaFor("team-a-quota", "team-a", true),
		policy("team-a-training", "team-a", true))
	if res.Allowed {
		t.Fatal("a namespace Kueue governs admitted a Job no queue would ever charge")
	}
	if !strings.Contains(res.Result.Message, kueueQueueLabel) {
		t.Fatalf("the refusal does not name the label the tenant must add: %s", res.Result.Message)
	}
}

// A policy saying false while its ResourceQuota is missing describes a namespace with no ceiling at all.
// That is the state the guard exists to refuse, not a licence to admit.
//
// Mutation that turns this red: trust policy.Spec.TrainingQuota alone and skip the ResourceQuota read.
func TestAMissingResourceQuotaIsNotAMeteredNamespace(t *testing.T) {
	res := askJob(t, unqueuedJob(), policy("team-a-quota", "team-a", false))
	if res.Allowed {
		t.Fatal("admitted a GPU Job into a namespace with neither a queue nor a ceiling")
	}
}

// A ResourceQuota that exists but caps something else is the same hole wearing the right name.
//
// Mutation that turns this red: check only that the ResourceQuota exists, not that it holds the GPU key.
func TestAQuotaWithoutTheGPUKeyIsNotAMeteredNamespace(t *testing.T) {
	res := askJob(t, unqueuedJob(),
		policy("team-a-quota", "team-a", false),
		gpuQuotaFor("team-a-quota", "team-a", false))
	if res.Allowed {
		t.Fatal("a ResourceQuota capping pods was read as a GPU ceiling")
	}
}

// A guard that stops applying when a read fails admits exactly what it exists to refuse, invisibly. The
// webhook's failurePolicy is Fail for the same reason.
//
// Mutation that turns this red: swallow the error from meteredByResourceQuota and treat it as false, or as
// true.
func TestAFailedQuotaModeReadRefusesRatherThanGuesses(t *testing.T) {
	// The owner Job reads back fine, so the guard reaches the quota-mode question and fails THERE.
	//
	// The first version of this test used erroringClient, whose Get fails — so tracesToAQueue errored first
	// and the quota-mode read was never reached. It passed while the branch it names was never executed,
	// which a mutation confirmed: swallowing the List error left the test green. Checking only !Allowed would
	// have hidden it even here, because swallowing the error as "not metered" still produces a refusal. The
	// assertion is therefore on the cause the refusal carries.
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "train-1", Namespace: "team-a"}}
	v := &GPUPodValidator{
		Reader:  listErroringReader{fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(job).Build()},
		decoder: admission.NewDecoder(scheme.Scheme),
	}
	res := ask(t, v, jobControllerUser, gpuPod(1, ownedBy("train-1")))
	if res.Allowed {
		t.Fatal("admitted a device request without establishing how the namespace meters GPUs")
	}
	if !strings.Contains(res.Result.Message, "meters GPUs") {
		t.Fatalf("the refusal does not carry the cause, so an outage reads as a policy decision: %s",
			res.Result.Message)
	}
}

// listErroringReader answers Get from a real fake client and fails every List.
//
// The guards read one object by name and list policies; separating the two is what lets a test reach the
// second read with the first one working.
type listErroringReader struct{ client.Reader }

func (listErroringReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errors.New("apiserver unreachable")
}

// The ResourceQuota path is an accounting decision. Grace is not: a Pod admitted through it holds its device
// against a reclaiming owner exactly as long as one admitted through a queue.
//
// Mutation that turns this red: move the meteredByResourceQuota branch above the graceTooLong check in the
// Job guard, which is how the Pod guard's own header records this going wrong once already.
func TestTheResourceQuotaPathIsStillCappedOnGrace(t *testing.T) {
	long := int64(600)
	j := gpuJob(2, func(j *batchv1.Job) {
		j.Labels = nil
		j.Spec.Template.Spec.TerminationGracePeriodSeconds = &long
	})
	res := askJob(t, j,
		policy("team-a-quota", "team-a", false),
		gpuQuotaFor("team-a-quota", "team-a", true))
	if res.Allowed {
		t.Fatal("a Job admitted through the ResourceQuota path escaped the grace cap")
	}
	if !strings.Contains(res.Result.Message, "600") {
		t.Fatalf("the refusal does not name the grace that caused it: %s", res.Result.Message)
	}
}

// quotaName is duplicated from internal/controller, and a guard looking for a differently named object finds
// nothing and concludes there is no cap — which refuses legitimate work rather than admitting it, but
// refuses it for a reason no tenant can act on.
//
// Mutation that turns this red: change either copy of the prefix.
func TestTheQuotaNameMatchesTheController(t *testing.T) {
	if got := quotaName("team-a-quota"); got != "gpuquota-team-a-quota" {
		t.Fatalf("the webhook looks for %q; GPUQuotaPolicyReconciler syncs gpuquota-<policy>", got)
	}
}
