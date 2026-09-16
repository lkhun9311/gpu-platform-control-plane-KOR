// Package v1 holds the admission webhooks for the platform v1 API.
//
// The rules here exist for one reason: the CRD schema accepts values the controller cannot act on. Each one
// closes a gap measured in internal/controller/mltrainingjob_immutable_test.go rather than a gap imagined
// from reading the types, and each rejection replaces an outcome that was silent.
package v1

import (
	"context"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// +kubebuilder:webhook:path=/validate-platform-lkhun9311-github-io-v1-mltrainingjob,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.lkhun9311.github.io,resources=mltrainingjobs,verbs=create;update,versions=v1,name=vmltrainingjob.kb.io,admissionReviewVersions=v1

// MLTrainingJobValidator rejects MLTrainingJob writes the controller could not honour.
//
// It holds a client because the update rule turns on whether the owned Job already exists, and that is the
// fact that decides whether an edit can still take effect. Reading status.phase instead would decide the
// same question from a field that lags the Job it describes.
type MLTrainingJobValidator struct {
	// Reader, not client.Client. Embedding the full interface put Create, Update, Delete and Patch on the
	// exported validator's own surface — an admission validator that can mutate the cluster is a wider public
	// API than the job needs, and it invites a future rule to write from inside a decision that should only
	// read. A named field also makes the dependency legible in tests: what has to be faked is one Get.
	//
	// It must be an UNCACHED reader, which is why SetupMLTrainingJobWebhookWithManager passes
	// mgr.GetAPIReader() rather than mgr.GetClient(). The manager's client reads through an informer cache,
	// and a cache that has not yet seen a Job created moments ago answers NotFound — not an error. That
	// answer is indistinguishable from the Job genuinely not existing, so the immutability rules below would
	// be skipped, silently, in precisely the window right after creation where an edit does the most damage.
	// The error path already fails closed; this is the path that had no error to fail on.
	Reader client.Reader
}

var _ admission.Validator[*platformv1.MLTrainingJob] = &MLTrainingJobValidator{}

// mltjGroupKind names the kind in the errors this validator returns.
var mltjGroupKind = schema.GroupKind{Group: platformv1.GroupVersion.Group, Kind: "MLTrainingJob"}

// SetupMLTrainingJobWebhookWithManager registers the validator with the manager's webhook server.
func SetupMLTrainingJobWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &platformv1.MLTrainingJob{}).
		WithValidator(newValidator(mgr)).
		Complete()
}

// newValidator picks the reader the validator runs on, and is separate from the registration above so a test
// can hold the result and check which one it got.
//
// The check that matters is an INEQUALITY against mgr.GetClient(), because asserting it equals
// mgr.GetAPIReader() only restates this line. The failure being guarded against is a revert to the cached
// client, and that is what an inequality catches. It cannot be caught any other way here: the bug needs the
// informer to lag behind a Create, and in envtest it never does, so the end-to-end spec passes under both
// readers.
func newValidator(mgr ctrl.Manager) *MLTrainingJobValidator {
	return &MLTrainingJobValidator{Reader: mgr.GetAPIReader()}
}

// ValidateCreate refuses a spec that could never produce a runnable Job.
func (v *MLTrainingJobValidator) ValidateCreate(
	_ context.Context, mltj *platformv1.MLTrainingJob,
) (admission.Warnings, error) {
	if errs := validateRunnable(&mltj.Spec); len(errs) > 0 {
		return nil, apierrors.NewInvalid(mltjGroupKind, mltj.Name, errs)
	}
	return nil, nil
}

// ValidateUpdate additionally refuses edits the controller has already stopped being able to apply.
func (v *MLTrainingJobValidator) ValidateUpdate(
	ctx context.Context, oldJob, newJob *platformv1.MLTrainingJob,
) (admission.Warnings, error) {
	// Once deletion has started, every update is allowed through.
	//
	// This validator already refuses to intercept DELETE, on the reasoning that a webhook able to refuse one
	// could strand an object nobody can remove. That reasoning was right and the guard was in the wrong place:
	// the finalizer is removed with an ordinary UPDATE, so an object whose spec this validator considers
	// unrunnable — an empty image on something created before this webhook existed — could never complete the
	// update that ends its deletion. It would sit in Terminating forever, refused by the rule that exists to
	// stop unrunnable jobs being CREATED.
	//
	// Nothing is lost by allowing these. The object is going away; a spec edit racing its own deletion changes
	// nothing that will ever run, and the rules below are about what a Job would do, not about cleanup.
	if !newJob.DeletionTimestamp.IsZero() {
		return nil, nil
	}

	errs := validateRunnable(&newJob.Spec)

	// The pod template is written once, when the Job is created, because a batch/v1 Job's template is
	// immutable afterwards. So the question is not whether the spec changed but whether the Job exists yet:
	// before it does, every field is still free to change.
	exists, err := v.ownedJobExists(ctx, newJob)
	if err != nil {
		// Failing closed. A rule that silently stopped applying whenever the API was briefly unreachable would
		// let through exactly the edits it exists to catch, and the webhook's failurePolicy is fail for the
		// same reason.
		return nil, fmt.Errorf("check whether the owned job exists for %s/%s: %w", newJob.Namespace, newJob.Name, err)
	}
	if exists {
		errs = append(errs, bakedFieldEdits(&oldJob.Spec, &newJob.Spec)...)
	}

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(mltjGroupKind, newJob.Name, errs)
	}
	return nil, nil
}

// ValidateDelete allows every delete.
//
// Teardown is the finalizer's business, and a webhook that could refuse a delete would be able to strand an
// object no one can remove.
func (v *MLTrainingJobValidator) ValidateDelete(
	_ context.Context, _ *platformv1.MLTrainingJob,
) (admission.Warnings, error) {
	return nil, nil
}

// validateRunnable rejects values the CRD schema admits and no Job could run with.
//
// +required means the field is PRESENT, not that it holds anything, so both of these arrive as empty
// strings through a schema that considers them valid.
func validateRunnable(spec *platformv1.MLTrainingJobSpec) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	if strings.TrimSpace(spec.Image) == "" {
		errs = append(errs, field.Required(specPath.Child("image"),
			"a training image is required; the Job would otherwise be created around a container that can never start"))
	}
	if strings.TrimSpace(spec.Queue) == "" {
		errs = append(errs, field.Required(specPath.Child("queue"),
			"a Kueue LocalQueue name is required; without one the workload is never admitted and the job waits forever"))
	}
	return errs
}

// bakedFieldEdits reports edits to the fields the owned Job's pod template was built from.
//
// Refusing these is what makes the edit visible. Permitting them is not permissive — the write is stored,
// the next reconcile succeeds, and the running Job keeps the old value, so the API and the cluster disagree
// with nothing anywhere reporting it. A rejection at this point says plainly that the change cannot be
// applied, which is the outcome the user can act on.
func bakedFieldEdits(oldSpec, newSpec *platformv1.MLTrainingJobSpec) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	const because = "the owned Job exists and its pod template is immutable; delete and recreate the MLTrainingJob to change this"

	if oldSpec.Image != newSpec.Image {
		errs = append(errs, field.Invalid(specPath.Child("image"), newSpec.Image, because))
	}
	if oldSpec.GPUClass != newSpec.GPUClass {
		errs = append(errs, field.Invalid(specPath.Child("gpuClass"), newSpec.GPUClass, because))
	}
	if oldSpec.GPUCount != newSpec.GPUCount {
		errs = append(errs, field.Invalid(specPath.Child("gpuCount"), newSpec.GPUCount, because))
	}
	if !slices.Equal(oldSpec.Command, newSpec.Command) {
		errs = append(errs, field.Invalid(specPath.Child("command"), newSpec.Command, because))
	}
	// Completions is immutable on a batch/v1 Job and parallelism is not, which is a distinction the reconciler
	// does not make: it writes both on every pass. Verified against a real apiserver rather than assumed —
	// parallelism goes from 2 to 4 without complaint, and completions is refused with "field is immutable".
	//
	// So an edit here was stored and then failed on every reconcile afterwards, indefinitely, and what the
	// user saw was a controller erroring about an object they had successfully updated.
	if oldSpec.Completions != newSpec.Completions {
		errs = append(errs, field.Invalid(specPath.Child("completions"), newSpec.Completions,
			"completions is immutable on the owned Job; delete and recreate the MLTrainingJob to change it"))
	}
	// Queue is not part of the pod template, so it needs its own sentence: the reconciler copies the desired
	// labels — including Kueue's queue-name label — onto the live Job on every pass, and Kueue's own webhook
	// refuses that label being changed on a Job it has already admitted. Without this rule the edit is stored
	// happily and then fails on every reconcile afterwards, which surfaces as a controller error loop about an
	// object the user believes they successfully updated.
	if oldSpec.Queue != newSpec.Queue {
		errs = append(errs, field.Invalid(specPath.Child("queue"), newSpec.Queue,
			"the owned Job is already admitted under queue "+oldSpec.Queue+
				"; delete and recreate the MLTrainingJob to move it"))
	}
	return errs
}

// ownedJobExists reports whether the reconciler has already created this MLTrainingJob's Job.
//
// The Job carries the MLTrainingJob's own name in its namespace, so this is a single Get rather than a list.
// A Job of that name owned by something else still counts: the reconciler refuses to adopt it and the
// template is not this object's to change either way.
func (v *MLTrainingJobValidator) ownedJobExists(ctx context.Context, mltj *platformv1.MLTrainingJob) (bool, error) {
	var job batchv1.Job
	err := v.Reader.Get(ctx, client.ObjectKey{Name: mltj.Name, Namespace: mltj.Namespace}, &job)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
