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

	batchv1 "k8s.io/api/batch/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-batch-v1-job,mutating=false,failurePolicy=fail,sideEffects=None,groups=batch,resources=jobs,verbs=create,versions=v1,name=vgpujob.kb.io,admissionReviewVersions=v1

// GPUJobValidator refuses a Job whose Pods this platform would never admit.
//
// It exists because refusing the Pod is too late. Kueue admits a Job by looking at the Job, and a Job whose
// Pods are rejected keeps its admission: the Job controller retries, every Pod is refused, and the Workload
// sits admitted holding quota.
//
// Measured on this cluster before this guard existed. A Job with terminationGracePeriodSeconds 600 took the
// ClusterQueue from used=1 to used=2 within twenty seconds and still held it four minutes later, with
// job.status.failed stuck at 0 — an admission rejection is not a Pod failure, so backoffLimit never advances
// and nothing ever gives the reservation back. A tenant submitting such Jobs holds a budget indefinitely
// while running nothing.
//
// So the rules that decide a Pod's fate are applied to the template that would produce it, at the point
// where the reservation is made rather than after.
type GPUJobValidator struct {
	decoder admission.Decoder
}

var _ admission.Handler = &GPUJobValidator{}

// SetupGPUJobWebhookWithManager registers the validator with the manager's webhook server.
func SetupGPUJobWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register("/validate-batch-v1-job", &webhook.Admission{
		Handler: &GPUJobValidator{decoder: admission.NewDecoder(mgr.GetScheme())},
	})
	return nil
}

// Handle refuses a Job that would reserve quota for Pods that cannot be created.
func (v *GPUJobValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var job batchv1.Job
	if err := v.decoder.Decode(req, &job); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	spec := &job.Spec.Template.Spec
	devices := gpuRequestOf(spec)
	if devices == 0 {
		return admission.Allowed("")
	}

	// The queue label is checked HERE and not on the Pod, because here it is answerable. Kueue reads it from
	// the Job, and a Job without it is never admitted — so its Pods are refused by the Pod guard for a reason
	// the user cannot act on from the Pod. Saying it at the Job is saying it where the field is.
	if job.Labels[kueueQueueLabel] == "" {
		return admission.Denied(fmt.Sprintf(
			"this Job asks for %d %s and carries no %s, so Kueue would never admit it and every Pod it "+
				"created would be refused: label it with the tenant's queue, or submit an MLTrainingJob, "+
				"which sets the label for you",
			devices, gpuResource, kueueQueueLabel))
	}
	if why := graceTooLong(spec); why != "" {
		return admission.Denied(why)
	}
	return admission.Allowed("")
}
