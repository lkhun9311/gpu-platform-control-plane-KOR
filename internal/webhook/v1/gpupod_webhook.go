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
	"net/http"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// gpuResource is the extended resource a Pod asks for when it wants a device.
const gpuResource = corev1.ResourceName("nvidia.com/gpu")

// kueueQueueLabel is the label Kueue reads to decide which LocalQueue a workload belongs to.
//
// Duplicated from internal/controller rather than imported, because a webhook importing the controller
// package to read one string would drag the whole reconciler into the admission binary's dependency graph.
const kueueQueueLabel = "kueue.x-k8s.io/queue-name"

// jobControllerUser is the identity the Job controller creates Pods as.
//
// It is the ONLY thing in an admission request that a tenant cannot write. Everything on the Pod — labels,
// annotations, and ownerReferences above all — is supplied by whoever creates it, and Kubernetes does not
// check that a named owner had anything to do with the object claiming it.
//
// That was not a theoretical weakness. The first version of this guard read the Pod's ownerReference and
// trusted it: a Pod naming a real, queued Job as its controller was admitted. Forging one took a single
// kubectl apply and obtained two devices, which is exactly the bypass the guard exists to close.
const jobControllerUser = "system:serviceaccount:kube-system:job-controller"

// systemNamespacePrefix identifies the service accounts allowed to use the exemption.
//
// A tenant can annotate their own Pod, so an exemption honoured for anyone is not an exemption, it is the
// bypass with an extra step. Infrastructure that legitimately holds a device outside the budget — a device
// plugin DaemonSet — runs as a kube-system service account, and a tenant cannot create one.
const systemNamespacePrefix = "system:serviceaccount:kube-system:"

// QuotaExemptAnnotation marks a Pod that may hold a device outside the tenant quota.
//
// It exists because some Pods legitimately need a device and cannot go through the queue: a device plugin
// DaemonSet, and — in this cluster's current configuration — the serving path, since Kueue's `pod` and
// `deployment` integrations are not enabled and a serving Pod is therefore invisible to it.
//
// It is an annotation someone has to write, not a silent hole. A guard whose exceptions are implicit is a
// guard nobody can audit: this way `kubectl get pods -o json | grep quota-exempt` enumerates every device
// this platform is not accounting for.
const QuotaExemptAnnotation = "platform.lkhun9311.github.io/quota-exempt"

// maxGraceSeconds caps how long a device-holding Pod may take to shut down.
//
// A quota is only worth what its RECLAIM is worth, and reclaim waits for termination. Measured on this
// cluster: a Pod with terminationGracePeriodSeconds 30 held its device for 32 seconds after deletion; the
// same Pod with 300 held it for 301. The field is set by the tenant being preempted, so without a cap the
// borrower decides how long it keeps a device its owner has already claimed back.
//
// That is not a hypothetical reading of the spec. The queuelab measured the same boundary from the other
// side: an unresponsive workload defeats reclaim entirely while its remaining service fits inside grace, and
// is cut off at exactly the grace period once it does not. Grace IS the bound, and it was tenant-controlled.
//
// 120 seconds because it has to be long enough for the workloads this platform is for. A training step that
// checkpoints on SIGTERM needs more than the 30-second default, and refusing that would push tenants to
// ignore SIGTERM instead — which is the behaviour that produces the worst outcome in the measurement above.
// The number is a policy choice and is stated here rather than derived, because there is nothing to derive
// it from until real workloads say how long their checkpoints take.
const maxGraceSeconds int64 = 120

// +kubebuilder:webhook:path=/validate--v1-pod,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=vgpupod.kb.io,admissionReviewVersions=v1

// GPUPodValidator refuses a Pod that would take a GPU without passing through the tenant's quota.
//
// The hole it closes is the platform's largest. When a GPUQuotaPolicy sets trainingQuota, the controller
// deliberately stops the namespace ResourceQuota from capping GPUs and makes the Kueue ClusterQueue the
// single authority — because otherwise the same GPU is charged twice and half the tenant's budget becomes
// unusable. But Kueue only sees what enters its queues. A Pod created directly, with a device request and no
// queue, is admitted by nobody and counted by nobody.
//
// That was not theoretical. On this cluster a bare Pod requesting two GPUs in a governed namespace was
// created, scheduled and Running, with no Kueue Workload anywhere. "Multi-tenant GPU quota" was a convention
// the platform's own controller followed, not a boundary it enforced.
type GPUPodValidator struct {
	// Reader is uncached, for the reason MLTrainingJobValidator's is: this validator reads the Job a Pod was
	// just created by, and an informer that has not caught up answers NotFound — which is indistinguishable
	// from the Job not existing and would let the Pod through.
	Reader client.Reader
	// decoder turns the admission request's raw bytes into a Pod.
	//
	// This is a raw Handler rather than a typed Validator because the decision needs the REQUESTER, and
	// admission.Validator is handed only the object. Everything on the object is attacker-controlled; the
	// requester is not.
	decoder admission.Decoder
}

var _ admission.Handler = &GPUPodValidator{}

// SetupGPUPodWebhookWithManager registers the validator with the manager's webhook server.
//
// Registered by path rather than through NewWebhookManagedBy, because that builder wires a typed Validator
// and this guard needs the admission request itself.
func SetupGPUPodWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register("/validate--v1-pod", &webhook.Admission{
		Handler: &GPUPodValidator{Reader: mgr.GetAPIReader(), decoder: admission.NewDecoder(mgr.GetScheme())},
	})
	return nil
}

// Handle refuses a device request that is not accounted for.
//
// The order is deliberate: cheapest and least surprising first, so the overwhelming majority of Pods — which
// ask for no device at all — leave immediately, and so a refusal is only ever reached by a Pod that really
// would take one.
func (v *GPUPodValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var pod corev1.Pod
	if err := v.decoder.Decode(req, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	devices := gpuRequest(&pod)
	if devices == 0 {
		return admission.Allowed("")
	}
	// Checked FIRST, above every branch that admits, because grace is a property of the Pod alone: it needs
	// no reads, no owner, and no queue, and it costs the reclaiming owner the same whichever of those decided
	// the Pod was allowed to exist.
	//
	// It used to sit further down, after the exemption branch and after the queue-label branch, and both of
	// those return Allowed. So a tenant applying a bare Pod with a queue label, two devices and
	// terminationGracePeriodSeconds: 3600 was admitted without the cap ever being consulted -- the exact
	// bypass this guard exists to close, reached by the exact mechanism its own thesis names, a guarantee
	// expressed in a field the tenant writes. The tests did not catch it because both grace tests came in as
	// the job controller and the queue-label test carried no grace.
	//
	// The cause was treating grace as part of the ACCOUNTING decision -- who pays for this device -- when it
	// is a decision about how long the device is held after someone else has already reclaimed it. Those are
	// independent questions, and ordering one behind the other is what let two early returns hide it. Hoisted
	// here it cannot be re-hidden by a branch added later, which is the property that matters more than the
	// three lines it moves.
	if why := graceTooLong(&pod.Spec); why != "" {
		return admission.Denied(why)
	}

	user := req.UserInfo.Username

	if _, exempt := pod.Annotations[QuotaExemptAnnotation]; exempt {
		// The annotation is honoured only for identities a tenant cannot assume. Honoured for anyone, it is
		// the bypass with an extra step: a tenant who can create a Pod can also annotate it.
		if !strings.HasPrefix(user, systemNamespacePrefix) {
			return admission.Denied(fmt.Sprintf(
				"%s is reserved for cluster infrastructure and %q is not a kube-system service account; "+
					"this Pod asks for %d %s and would leave the tenant budget unaccounted",
				QuotaExemptAnnotation, user, devices, gpuResource))
		}
		return admission.Allowed("").WithWarnings(fmt.Sprintf(
			"this Pod holds %d %s outside the tenant quota, by %s, created by %s",
			devices, gpuResource, QuotaExemptAnnotation, user))
	}

	// A Pod carrying the queue label is one Kueue itself is accounting for, and it needs no owner at all.
	//
	// This branch exists because the first version did not have it and was built on a claim that turned out to
	// be false: that Kueue's `pod` and `deployment` integrations were not enabled here, so only a Job could be
	// governed. They ARE enabled — seventeen frameworks including pod, deployment and statefulset — and the
	// conclusion came from reading the first twelve lines of a truncated grep.
	//
	// What that changes is the meaning of the label. It is not a permission a tenant grants itself; it is a
	// BILL. A bare Pod labelled kueue.x-k8s.io/queue-name gets a Kueue Workload and is charged against that
	// ClusterQueue — verified by creating one and watching the admitted Workload appear. Forging it does not
	// obtain free capacity, it obtains metered capacity, which is the whole objective.
	//
	// The label is checked and not kueue.x-k8s.io/managed, which is forgeable in the way that matters: a Pod
	// carrying managed=true and no queue name runs with no Workload at all.
	if pod.Labels[kueueQueueLabel] != "" {
		return admission.Allowed("")
	}

	// Otherwise the Pod must come from a Job, because Kueue governs a Job through the JOB object and does not
	// propagate the label down: a Pod the Job controller created for a queued Job carries neither
	// queue-name nor managed. Verified rather than assumed — the Deployment path does carry both.
	//
	// An ownerReference is only evidence when the Job controller is the one presenting it. A tenant writing
	// the same field is describing a relationship that does not exist.
	if user != jobControllerUser {
		return admission.Denied(fmt.Sprintf(
			"this namespace's GPU budget is held by a Kueue ClusterQueue, and %q is asking for %d %s directly "+
				"rather than through a queue, so nothing would charge it: submit an MLTrainingJob, or create a "+
				"Job labelled %s=<queue> and let the Job controller create the Pod",
			user, devices, gpuResource, kueueQueueLabel))
	}

	queued, err := v.tracesToAQueue(ctx, &pod)
	if err != nil {
		// Fail closed, for the same reason the webhook's failurePolicy is Fail: a guard that stops applying
		// whenever a read fails admits exactly what it exists to refuse, and does so invisibly.
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf(
			"establish whether pod %s/%s passes through a queue: %w", pod.Namespace, pod.Name, err))
	}
	if !queued {
		return admission.Denied(fmt.Sprintf(
			"the Job that created this Pod carries no %s, so the %d %s it asks for would be charged to no "+
				"tenant's budget", kueueQueueLabel, devices, gpuResource))
	}
	return admission.Allowed("")
}

// tracesToAQueue reports whether this Pod reaches Kueue's accounting through its controller.
//
// The label is looked for on the OWNER rather than on the Pod, and that is not a detail. BuildJob puts the
// queue label on the Job's own metadata; Pods are created by the Job controller from spec.template, which
// carries no such label. A guard reading the Pod alone would refuse every legitimate training Pod this
// platform creates.
//
// Only a batch/v1 Job owner is accepted, and the reason written here used to be FALSE: it said Kueue's `pod`
// and `deployment` integrations were not among the enabled frameworks. They are -- seventeen of them -- and
// the correction was already made forty lines above, in the branch that admits a queue-labelled Pod, while
// this sentence went on asserting the retracted claim underneath it. One file, two contradictory statements
// about the same cluster, and the newer one was load-bearing.
//
// The real reason is narrower and does not depend on which integrations are on. Everything reaching this
// function was created by the Job controller, because the identity check above admits nothing else, and the
// Job controller creates Pods owned by Jobs. A non-Job owner here therefore describes a relationship that
// cannot have been formed by the requester the Pod claims to come from, and refusing it is the safe reading
// of a contradiction rather than a claim about Kueue's configuration.
func (v *GPUPodValidator) tracesToAQueue(ctx context.Context, pod *corev1.Pod) (bool, error) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return false, nil
	}
	if owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() {
		return false, nil
	}
	var job batchv1.Job
	err := v.Reader.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: pod.Namespace}, &job)
	switch {
	case apierrors.IsNotFound(err):
		// The Pod names an owner that does not exist. Refusing is the safe reading: the alternative is
		// trusting an ownerReference anyone can write to describe a Job nobody can check.
		return false, nil
	case err != nil:
		return false, err
	}
	return job.Labels[kueueQueueLabel] != "", nil
}

// graceTooLong returns why this spec's termination grace exceeds the cap, or "" when it does not.
//
// Shared by the Pod guard and the Job guard so the two cannot drift: a Job whose template would produce a
// Pod this rule refuses must be refused itself, or it reserves quota for Pods that can never be created.
func graceTooLong(spec *corev1.PodSpec) string {
	g := spec.TerminationGracePeriodSeconds
	if g == nil || *g <= maxGraceSeconds {
		return ""
	}
	return fmt.Sprintf(
		"terminationGracePeriodSeconds is %d, and this namespace's GPU budget is reclaimed by preemption — a "+
			"Pod that takes %d seconds to stop holds its device that long against the owner that reclaimed "+
			"it. The cap here is %d", *g, *g, maxGraceSeconds)
}

// gpuRequest is how many devices this Pod would hold.
//
// Limits rather than requests, because an extended resource's request is defaulted from its limit and a Pod
// may set only the limit — which is exactly what BuildJob does.
func gpuRequest(pod *corev1.Pod) int64 { return gpuRequestOf(&pod.Spec) }

// gpuRequestOf is the same question asked of a bare PodSpec, which is what a Job carries.
func gpuRequestOf(spec *corev1.PodSpec) int64 {
	var total int64
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if q, ok := c.Resources.Limits[gpuResource]; ok {
			total += q.Value()
			continue
		}
		if q, ok := c.Resources.Requests[gpuResource]; ok {
			total += q.Value()
		}
	}
	return total
}
