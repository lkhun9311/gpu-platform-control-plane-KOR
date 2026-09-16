package v1

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// job builds a valid MLTrainingJob the individual specs then break in one way each.
func job(mutate func(*platformv1.MLTrainingJob)) *platformv1.MLTrainingJob {
	mltj := &platformv1.MLTrainingJob{
		ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "default"},
		Spec: platformv1.MLTrainingJobSpec{
			Queue: "team-a", Image: "trainer:v1", GPUCount: 1, Parallelism: 1, Completions: 1,
		},
	}
	if mutate != nil {
		mutate(mltj)
	}
	return mltj
}

// validatorWith builds a validator whose client holds the given objects.
func validatorWith(t *testing.T, objs ...runtime.Object) *MLTrainingJobValidator {
	t.Helper()
	if err := platformv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return &MLTrainingJobValidator{
		Reader: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build(),
	}
}

// ownedJob is the Job the reconciler creates, which is what makes the template-derived fields unchangeable.
func ownedJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "default"}}
}

// causes lists the field paths an Invalid error blames.
func causes(t *testing.T, err error) []string {
	t.Helper()
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		t.Fatalf("expected a structured API error, got %v", err)
	}
	fields := make([]string, 0, len(status.Status().Details.Causes))
	for _, c := range status.Status().Details.Causes {
		fields = append(fields, c.Field)
	}
	return fields
}

func TestValidateCreateRejectsWhatTheSchemaAdmits(t *testing.T) {
	// Both of these reach the webhook as empty strings through a schema that considers them valid, because
	// +required means the field is PRESENT rather than populated. internal/controller's immutability specs
	// assert the apiserver really does accept them, so these rows close a measured gap rather than a guessed one.
	//
	// Mutation that turns this red: drop either TrimSpace check from validateRunnable.
	rows := []struct {
		name   string
		break_ func(*platformv1.MLTrainingJob)
		want   string
	}{
		{"blank image", func(m *platformv1.MLTrainingJob) { m.Spec.Image = "" }, "spec.image"},
		{"whitespace image", func(m *platformv1.MLTrainingJob) { m.Spec.Image = "   " }, "spec.image"},
		{"blank queue", func(m *platformv1.MLTrainingJob) { m.Spec.Queue = "" }, "spec.queue"},
		{"whitespace queue", func(m *platformv1.MLTrainingJob) { m.Spec.Queue = "\t\n" }, "spec.queue"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			v := validatorWith(t)
			_, err := v.ValidateCreate(context.Background(), job(row.break_))
			if err == nil {
				t.Fatalf("accepted %s, which no Job could run with", row.name)
			}
			if got := causes(t, err); len(got) != 1 || got[0] != row.want {
				t.Fatalf("blamed %v, want exactly [%s]", got, row.want)
			}
		})
	}
}

func TestValidateCreateAcceptsARunnableSpec(t *testing.T) {
	// The control. Without it, a validator that rejected everything would pass every rejection spec above.
	v := validatorWith(t)
	if _, err := v.ValidateCreate(context.Background(), job(nil)); err != nil {
		t.Fatalf("rejected a spec with an image and a queue: %v", err)
	}
}

func TestValidateUpdateAllowsBakedEditsBeforeTheJobExists(t *testing.T) {
	// Before the Job is created there is nothing the reconciler cannot still apply, so the immutability rule
	// must not fire. This is the control that separates "refuses edits it cannot apply" from "refuses edits".
	//
	// Mutation that turns this red: drop the ownedJobExists check and always append bakedFieldEdits.
	v := validatorWith(t)
	updated := job(func(m *platformv1.MLTrainingJob) { m.Spec.Image = "trainer:v2"; m.Spec.GPUCount = 4 })
	if _, err := v.ValidateUpdate(context.Background(), job(nil), updated); err != nil {
		t.Fatalf("refused an edit made before the Job existed: %v", err)
	}
}

func TestValidateUpdateRefusesBakedEditsOnceTheJobExists(t *testing.T) {
	// Each of these is silently ignored today: the write is stored, the next reconcile reports success, and
	// the running Job keeps the old value. Rejecting is what makes the divergence visible.
	//
	// Mutation that turns this red: remove any one field's comparison from bakedFieldEdits.
	rows := []struct {
		name string
		edit func(*platformv1.MLTrainingJob)
		want string
	}{
		{"image", func(m *platformv1.MLTrainingJob) { m.Spec.Image = "trainer:v2" }, "spec.image"},
		{"gpuCount", func(m *platformv1.MLTrainingJob) { m.Spec.GPUCount = 8 }, "spec.gpuCount"},
		{"gpuClass", func(m *platformv1.MLTrainingJob) { m.Spec.GPUClass = "h100" }, "spec.gpuClass"},
		{"command", func(m *platformv1.MLTrainingJob) { m.Spec.Command = []string{"python", "train.py"} }, "spec.command"},
		// Queue is not in the pod template, so it fails differently from the four above: the reconciler DOES
		// re-apply it, on every pass, and Kueue's own webhook then refuses the change on a Job it has already
		// admitted. The edit is stored and the controller loops on it forever, which reads as a controller bug
		// rather than as the rejected edit it is.
		{"queue", func(m *platformv1.MLTrainingJob) { m.Spec.Queue = "team-b" }, "spec.queue"},
		// Completions is immutable on a batch/v1 Job; parallelism is not. Confirmed against a real apiserver
		// in envtest, which accepts a parallelism change and answers "field is immutable" for completions.
		{"completions", func(m *platformv1.MLTrainingJob) { m.Spec.Completions = 4 }, "spec.completions"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			v := validatorWith(t, ownedJob())
			_, err := v.ValidateUpdate(context.Background(), job(nil), job(row.edit))
			if err == nil {
				t.Fatalf("accepted an edit to %s that the running Job would never pick up", row.name)
			}
			if got := causes(t, err); len(got) != 1 || got[0] != row.want {
				t.Fatalf("blamed %v, want exactly [%s]", got, row.want)
			}
		})
	}
}

func TestValidateUpdateAllowsFieldsTheReconcilerStillSyncs(t *testing.T) {
	// Parallelism is written on EVERY reconcile and the apiserver accepts the change, so refusing it would
	// block something the controller honours. Completions is written on every reconcile too and the apiserver
	// does NOT accept it — the reconciler makes no distinction and this spec used to repeat that mistake by
	// asserting both were allowed.
	//
	// Mutation that turns this red: add Parallelism to bakedFieldEdits.
	v := validatorWith(t, ownedJob())
	updated := job(func(m *platformv1.MLTrainingJob) { m.Spec.Parallelism = 4 })
	if _, err := v.ValidateUpdate(context.Background(), job(nil), updated); err != nil {
		t.Fatalf("refused an edit the reconciler re-applies every pass: %v", err)
	}
}

// erroringClient fails every Get, standing in for an apiserver that is briefly unreachable.
type erroringClient struct {
	client.Client
}

func (erroringClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return errors.New("apiserver unreachable")
}

func TestValidateUpdateFailsClosedWhenTheJobLookupFails(t *testing.T) {
	// A rule that quietly stopped applying whenever the lookup failed would let through exactly the edits it
	// exists to catch, and it would do so at the least visible moment.
	//
	// Mutation that turns this red: treat a lookup error as "no Job exists" and carry on.
	v := &MLTrainingJobValidator{Reader: erroringClient{}}
	_, err := v.ValidateUpdate(context.Background(), job(nil), job(func(m *platformv1.MLTrainingJob) {
		m.Spec.Image = "trainer:v2"
	}))
	if err == nil {
		t.Fatal("admitted an edit without establishing whether the Job existed")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error lost the cause: %v", err)
	}
}

// A cached read answers NotFound for a Job that exists but has not reached the informer yet, and NotFound is
// not an error — so the fail-closed path above never runs and the immutability rules are simply skipped.
//
// This spec pins the CONSEQUENCE of reading through a lagging cache, which is why the validator is wired with
// mgr.GetAPIReader() rather than mgr.GetClient(). It is the shape of the bug, held still: an edit that must be
// refused is admitted, with no error anywhere, in the window right after creation where it does the most harm.
//
// Mutation that turns this red: make the rules apply regardless of whether the Job was found, which would
// also refuse legitimate pre-creation edits.
func TestALaggingReadSilentlySkipsTheImmutabilityRules(t *testing.T) {
	// The Job exists on the cluster; this reader has not seen it. No error is produced, which is the whole
	// problem — there is nothing for the fail-closed branch to catch.
	v := &MLTrainingJobValidator{Reader: coldCache{}}

	_, err := v.ValidateUpdate(context.Background(), job(nil), job(func(m *platformv1.MLTrainingJob) {
		m.Spec.Image = "trainer:v2"
	}))
	if err != nil {
		t.Fatalf("a reader that reports NotFound should skip the baked-field rules, so this edit is admitted "+
			"under it; the spec exists to record that, not to assert the opposite: %v", err)
	}

	// And with a reader that can see it, the same edit is refused. The two together say the outcome turns
	// entirely on whether the read was current.
	warm := validatorWith(t, ownedJob())
	if _, err := warm.ValidateUpdate(context.Background(), job(nil), job(func(m *platformv1.MLTrainingJob) {
		m.Spec.Image = "trainer:v2"
	})); err == nil {
		t.Fatal("an edit to a baked field was admitted even though the Job was visible")
	}
}

// coldCache reports every Job as absent, standing in for an informer that has not caught up.
type coldCache struct {
	client.Client
}

func (coldCache) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, key.Name)
}

func TestValidateDeleteAllowsEveryDelete(t *testing.T) {
	// Teardown belongs to the finalizer. A webhook able to refuse a delete could strand an object no one can
	// remove, which is a worse failure than anything it would prevent.
	v := validatorWith(t, ownedJob())
	if _, err := v.ValidateDelete(context.Background(), job(nil)); err != nil {
		t.Fatalf("refused a delete: %v", err)
	}
}

// An object being deleted must be able to finish being deleted, whatever its spec says.
//
// This validator refuses to intercept DELETE precisely so it cannot strand an object. That guard was in the
// wrong place: the finalizer comes off with an ordinary UPDATE, so a spec this validator calls unrunnable —
// an empty image on something created before the webhook existed — could never complete the update that ends
// its deletion. The object would sit in Terminating with no way out, held there by a rule about what a Job
// would run.
//
// Mutation that turns this red: drop the DeletionTimestamp early return from ValidateUpdate.
func TestValidateUpdateNeverBlocksDeletion(t *testing.T) {
	deleting := func(mutate func(*platformv1.MLTrainingJob)) *platformv1.MLTrainingJob {
		now := metav1.Now()
		j := job(mutate)
		j.DeletionTimestamp = &now
		j.Finalizers = []string{"platform.lkhun9311.github.io/mltrainingjob"}
		return j
	}

	rows := []struct {
		name string
		spec func(*platformv1.MLTrainingJob)
	}{
		// The legacy shapes: created before this webhook, so they hold values it would refuse today.
		{"empty image", func(m *platformv1.MLTrainingJob) { m.Spec.Image = "" }},
		{"empty queue", func(m *platformv1.MLTrainingJob) { m.Spec.Queue = "" }},
		{"both empty", func(m *platformv1.MLTrainingJob) { m.Spec.Image = ""; m.Spec.Queue = "" }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// The owned Job exists, so the immutability rule would also fire on an ordinary update.
			v := validatorWith(t, ownedJob())
			old := deleting(row.spec)
			// What the controller actually does: same object, one finalizer fewer.
			updated := deleting(row.spec)
			updated.Finalizers = nil

			if _, err := v.ValidateUpdate(context.Background(), old, updated); err != nil {
				t.Fatalf("finalizer removal was refused, so the object can never be deleted: %v", err)
			}
		})
	}

	// The control: with no deletion under way the same spec must still be refused, or the bypass has swallowed
	// the rule rather than scoped it.
	v := validatorWith(t, ownedJob())
	broken := job(func(m *platformv1.MLTrainingJob) { m.Spec.Image = "" })
	if _, err := v.ValidateUpdate(context.Background(), job(nil), broken); err == nil {
		t.Fatal("an empty image was accepted on a live object; the deletion bypass is too wide")
	}
}
