package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func gpuJob(devices int64, mutate func(*batchv1.Job)) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j1", Namespace: "team-a",
			Labels: map[string]string{kueueQueueLabel: "team-a-queue"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "trainer", Image: "trainer:v1",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					gpuResource: *resource.NewQuantity(devices, resource.DecimalSI)}}}},
		}}},
	}
	if mutate != nil {
		mutate(j)
	}
	return j
}

func askJob(t *testing.T, j *batchv1.Job) admission.Response {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	v := &GPUJobValidator{decoder: admission.NewDecoder(scheme.Scheme)}
	return v.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}})
}

// Refusing the Pod is too late. Measured before this guard existed: a Job with grace 600 took the
// ClusterQueue from used=1 to used=2 within twenty seconds and still held it four minutes later, with
// job.status.failed stuck at 0 — an admission rejection is not a Pod failure, so backoffLimit never advances
// and nothing gives the reservation back.
//
// Mutation that turns this red: drop the grace check from the Job guard.
func TestAJobWhosePodsWouldBeRefusedIsRefusedItself(t *testing.T) {
	long := int64(600)
	res := askJob(t, gpuJob(1, func(j *batchv1.Job) {
		j.Spec.Template.Spec.TerminationGracePeriodSeconds = &long
	}))
	if res.Allowed {
		t.Fatal("a Job reserved quota for Pods that can never be created")
	}
	if !strings.Contains(res.Result.Message, "600") {
		t.Fatalf("the refusal does not name the value asked for: %s", res.Result.Message)
	}
}

// The queue label is answerable at the Job and not at the Pod, because Kueue reads it from the Job. Refusing
// at the Pod would tell a user to fix a field their Pod does not have.
//
// Mutation that turns this red: drop the queue-label check from the Job guard.
func TestAGPUJobWithNoQueueIsRefused(t *testing.T) {
	res := askJob(t, gpuJob(2, func(j *batchv1.Job) { j.Labels = nil }))
	if res.Allowed {
		t.Fatal("a Job Kueue would never admit was allowed to reserve nothing and retry forever")
	}
	if !strings.Contains(res.Result.Message, "MLTrainingJob") {
		t.Fatalf("the refusal offers no route that works: %s", res.Result.Message)
	}
}

func TestAnOrdinaryQueuedGPUJobIsAdmitted(t *testing.T) {
	ok := int64(90)
	res := askJob(t, gpuJob(1, func(j *batchv1.Job) {
		j.Spec.Template.Spec.TerminationGracePeriodSeconds = &ok
	}))
	if !res.Allowed {
		t.Fatalf("refused an ordinary training Job: %s", res.Result.Message)
	}
}

// A Job holding no device reserves no GPU budget, so neither rule reaches it — including the queue label,
// which plenty of legitimate non-GPU Jobs will not carry.
func TestAJobWithNoDeviceRequestIsUntouched(t *testing.T) {
	res := askJob(t, gpuJob(0, func(j *batchv1.Job) {
		j.Labels = nil
		j.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}
	}))
	if !res.Allowed {
		t.Fatalf("a Job asking for no device was refused: %s", res.Result.Message)
	}
}
