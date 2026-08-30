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

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// nvidiaGPUResource is the node extended resource a serving pod requests per GPU.
//
// This is the pod-level resource, distinct from the GPUQuotaPolicy ResourceQuota key.
const nvidiaGPUResource = corev1.ResourceName("nvidia.com/gpu")

const (
	// instanceLabel selects the pods owned by one InferenceDeployment.
	//
	// It is set once and never changed, because a Deployment's selector is immutable.
	instanceLabel = "app.kubernetes.io/instance"
)

// InferenceDeploymentReconciler reconciles an InferenceDeployment object.
type InferenceDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

// Reconcile syncs a Deployment and Service from the InferenceDeployment and reflects readiness into status.
func (r *InferenceDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var infd platformv1.InferenceDeployment
	if err := r.Get(ctx, req.NamespacedName, &infd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if conflict, err := r.ownedConflict(ctx, &infd, &appsv1.Deployment{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check deployment ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		log.Info("Deployment exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonConflict, "a Deployment of the same name is not owned by this InferenceDeployment")
	}

	// Resolved before the mutation rather than inside it, because CreateOrUpdate's callback runs more than
	// once on conflict and a List per attempt would multiply reads for an answer that cannot change mid-call.
	queue, err := r.servingQueue(ctx, infd.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		r.mutateDeployment(&infd, dep, queue)
		return controllerutil.SetControllerReference(&infd, dep, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync deployment %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	if conflict, err := r.ownedConflict(ctx, &infd, &corev1.Service{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check service ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		log.Info("Service exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonServiceConflict, "a Service of the same name is not owned by this InferenceDeployment")
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		r.mutateService(&infd, svc)
		return controllerutil.SetControllerReference(&infd, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync service %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	log.Info("Synced serving objects", "inferenceDeployment", req.String())

	phase, cond := computeInfDPhase(&infd, dep)
	desired := infd.Status.DeepCopy()
	desired.Phase = phase
	desired.ReadyReplicas = dep.Status.ReadyReplicas
	desired.ObservedGeneration = infd.Generation
	meta.SetStatusCondition(&desired.Conditions, cond)

	if !equality.Semantic.DeepEqual(infd.Status, *desired) {
		infd.Status = *desired
		if err := r.Status().Update(ctx, &infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s: %w", infd.Namespace, infd.Name, err)
		}
		log.Info("Updated InferenceDeployment status", "name", infd.Name, "phase", phase)
	}
	return ctrl.Result{}, nil
}

// servingPort returns the configured serving port, defaulting to 8080 when unset.
func servingPort(infd *platformv1.InferenceDeployment) int32 {
	if infd.Spec.Port == 0 {
		return 8080
	}
	return infd.Spec.Port
}

// servingQueue is the LocalQueue this namespace's serving Pods are charged against, or "" when no policy
// governs it.
//
// A miss is not an error: a namespace with no GPUQuotaPolicy is one this platform makes no quota claim about,
// and refusing to reconcile serving there would make the policy a prerequisite for running at all.
func (r *InferenceDeploymentReconciler) servingQueue(ctx context.Context, ns string) (string, error) {
	var policies platformv1.GPUQuotaPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return "", fmt.Errorf("list quota policies: %w", err)
	}
	for i := range policies.Items {
		p := &policies.Items[i]
		if p.Spec.TargetNamespace == ns && p.Spec.TrainingQuota {
			return kueueQueueName(p.Spec.Tenant), nil
		}
	}
	return "", nil
}

// infdLabels is the recommended label set applied to the owned Deployment and Service.
func infdLabels(infd *platformv1.InferenceDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":              "inferencedeployment",
		instanceLabel:                         infd.Name,
		"app.kubernetes.io/managed-by":        "gpu-platform-control-plane",
		"platform.lkhun9311.github.io/tenant": infd.Namespace,
	}
}

// mutateDeployment sets only the fields this controller manages on the Deployment.
//
// The selector is set once and never changed, because it is immutable after create.
func (r *InferenceDeploymentReconciler) mutateDeployment(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment, queue string) {
	labels := infdLabels(infd)
	// The queue label puts this Deployment's Pods on the tenant's quota, and Kueue propagates it down to them
	// along with its own managed marker.
	//
	// It is derived from the namespace's GPUQuotaPolicy rather than taken from the InferenceDeployment spec,
	// which is the point: a tenant naming its own queue could name another tenant's and be charged against
	// somebody else's budget. Serving does not get to choose which budget it spends.
	//
	// Empty when no policy governs this namespace, in which case nothing is charged and nothing claims to be.
	if queue != "" {
		labels[kueueQueueLabel] = queue
	}
	port := servingPort(infd)

	dep.Labels = labels
	dep.Spec.Replicas = new(infd.Spec.Replicas)
	dep.Spec.ProgressDeadlineSeconds = new(int32(600))
	if dep.Spec.Selector == nil {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: infd.Name}}
	}
	dep.Spec.Template.Labels = labels

	container := corev1.Container{
		Name:  "server",
		Image: infd.Spec.Image,
		Args:  []string{"--model", infd.Spec.Model.Name, "--model-path", infd.Spec.Model.StorageURI},
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
	}
	if infd.Spec.GPUCount > 0 {
		q := *resource.NewQuantity(int64(infd.Spec.GPUCount), resource.DecimalSI)
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{nvidiaGPUResource: q},
			Limits:   corev1.ResourceList{nvidiaGPUResource: q},
		}
	}
	dep.Spec.Template.Spec.Containers = []corev1.Container{container}
}

// mutateService sets only the fields this controller manages on the Service.
func (r *InferenceDeploymentReconciler) mutateService(infd *platformv1.InferenceDeployment, svc *corev1.Service) {
	port := servingPort(infd)
	svc.Labels = infdLabels(infd)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Selector = map[string]string{instanceLabel: infd.Name}
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       port,
		TargetPort: intstr.FromString("http"),
	}}
}

const (
	infdPhasePending     = "Pending"
	infdPhaseProgressing = "Progressing"
	infdPhaseReady       = "Ready"
	infdPhaseDegraded    = "Degraded"

	infdCondAvailable         = "Available"
	infdReasonScaledZero      = "ScaledToZero"
	infdReasonRollout         = "RolloutInProgress"
	infdReasonAvailable       = "MinimumReplicasAvailable"
	infdReasonConflict        = "DeploymentConflict"
	infdReasonServiceConflict = "ServiceConflict"
)

// computeInfDPhase derives the phase and the Available condition from the Deployment status.
//
// Precedence (top to bottom):
//
//  1. stale gate: when spec.Replicas > 0 and ObservedGeneration < Generation → Progressing.
//     This ensures a scale-down to 0 not yet observed by the Deployment does not prematurely report Ready.
//  2. ScaledToZero: Replicas == 0 → Ready (zero replicas is always authoritative from spec alone).
//  3. Degraded: Deployment Progressing condition is False with ProgressDeadlineExceeded.
//  4. Pending: ReadyReplicas == 0.
//  5. Progressing: UpdatedReplicas or ReadyReplicas < spec.Replicas, old replicas not yet drained,
//     or the three replica counts have not yet converged on spec.Replicas.
//  6. Ready: fully converged.
func computeInfDPhase(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment) (string, metav1.Condition) {
	avail := func(status metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: infdCondAvailable, Status: status, Reason: reason, Message: msg, ObservedGeneration: infd.Generation}
	}
	// 1. Stale gate: block phase decisions until the Deployment controller has observed the current spec.
	//
	// The gate is skipped only when spec wants zero replicas and status already shows zero running replicas, because there is nothing to drain and the spec intent is authoritative.
	staleDep := dep.Status.ObservedGeneration < dep.Generation
	zeroAndDrained := infd.Spec.Replicas == 0 && dep.Status.Replicas == 0
	if staleDep && !zeroAndDrained {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "deployment not yet observed")
	}
	// 2. ScaledToZero: intentional zero-replica state is Ready once the status shows no running replicas.
	if infd.Spec.Replicas == 0 {
		return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonScaledZero, "scaled to zero replicas")
	}
	// 3. Degraded: a ProgressDeadlineExceeded condition means the rollout will never complete on its own.
	for i := range dep.Status.Conditions {
		c := dep.Status.Conditions[i]
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return infdPhaseDegraded, avail(metav1.ConditionFalse, "ProgressDeadlineExceeded", "deployment rollout failed")
		}
	}
	// 4. Pending: no replicas are ready yet.
	if dep.Status.ReadyReplicas == 0 {
		return infdPhasePending, avail(metav1.ConditionFalse, infdReasonRollout, "no replicas ready yet")
	}
	// 5. Progressing: replicas are not yet fully updated/ready, or old replicas have not been drained.
	if dep.Status.UpdatedReplicas < infd.Spec.Replicas || dep.Status.ReadyReplicas < infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "rollout in progress")
	}
	if dep.Status.Replicas != dep.Status.UpdatedReplicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for old replicas to drain")
	}
	// Still Progressing: all three replica counts must equal the desired count to guard against a scale-down in progress where surplus replicas have not yet been removed.
	if dep.Status.Replicas != infd.Spec.Replicas || dep.Status.UpdatedReplicas != infd.Spec.Replicas || dep.Status.ReadyReplicas != infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for replica count to converge")
	}
	// 6. Ready: fully converged.
	return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonAvailable, "all replicas ready")
}

// markDegraded reflects a deterministic failure into status as Degraded with Available=False.
//
// ReadyReplicas is zeroed so a DeploymentConflict does not leave a stale ready count.
//
// It returns a RequeueAfter so the deployment recovers automatically once the blocking condition clears, since the conflicting Deployment or Service is by definition not owned and so does not trigger the Owns watch.
func (r *InferenceDeploymentReconciler) markDegraded(ctx context.Context, infd *platformv1.InferenceDeployment, reason, msg string) (ctrl.Result, error) {
	desired := infd.Status.DeepCopy()
	desired.Phase = infdPhaseDegraded
	desired.ReadyReplicas = 0
	desired.ObservedGeneration = infd.Generation
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: infdCondAvailable, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: infd.Generation,
	})
	if !equality.Semantic.DeepEqual(infd.Status, *desired) {
		infd.Status = *desired
		if err := r.Status().Update(ctx, infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s to Degraded: %w", infd.Namespace, infd.Name, err)
		}

		// Count the transition only after the status write succeeds, so a reconcile that finds the object already Degraded does not inflate the metric.
		inferenceDeploymentDegradedTotal.WithLabelValues(reason).Inc()
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// ownedConflict reports whether an object of the given name exists but is not controlled by infd.
func (r *InferenceDeploymentReconciler) ownedConflict(ctx context.Context, infd *platformv1.InferenceDeployment, obj client.Object) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: infd.Name, Namespace: infd.Namespace}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	default:
		return !metav1.IsControlledBy(obj, infd), nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.InferenceDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("inferencedeployment").
		Complete(r)
}
