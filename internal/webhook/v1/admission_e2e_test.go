package v1

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/yaml"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// This is the test the unit tests cannot be.
//
// Everything above calls the validator's methods directly, so it proves the RULES are right and proves
// nothing about whether an apiserver ever reaches them. The path from a kubectl apply to ValidateCreate runs
// through the ValidatingWebhookConfiguration's rules, its resource and operation lists, and the handler path
// the marker declares — and every one of those can be wrong while the Go code is perfect.
//
// envtest installs config/webhook/manifests.yaml into a real apiserver and rewrites only the clientConfig to
// dial a local server, so the parts that decide whether the webhook is consulted are the repository's own.

// startAdmissionEnv brings up an apiserver with the real webhook manifest installed and the validator serving.
func startAdmissionEnv(t *testing.T) (context.Context, client.Client) {
	t.Helper()

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "..", "test", "crd", "kueue"),
		},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "config", "webhook", "manifests.yaml")},
		},
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}

	cfg, err := env.Start()
	if err != nil {
		t.Skipf("envtest unavailable, skipping admission end-to-end: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	if err := platformv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := env.WebhookInstallOptions
	mgr, err := manager.New(cfg, manager.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	registerEveryWebhookTheManifestDeclares(t, mgr)
	// A precondition of every spec below rather than a spec of its own, because it is about how the validator
	// is BUILT and the specs are about what it decides.
	//
	// The manager's client reads through an informer cache, which answers NotFound for a Job created moments
	// ago — not an error, so the fail-closed branch never runs and the immutability rules are skipped exactly
	// when they matter. The timing itself cannot be reproduced here: envtest's cache is never slow enough, and
	// TestApiserverRefusesAnImageEditOnceTheJobExists passes under either reader. This is what catches a
	// revert.
	if got := newValidator(mgr).Reader; got == client.Reader(mgr.GetClient()) {
		t.Fatal("the validator reads through the manager's cache, so a Job the informer has not seen yet " +
			"reads as absent and the baked-field rules are silently skipped")
	}
	go func() { _ = mgr.Start(ctx) }()

	waitForWebhookServer(t, opts.LocalServingHost, opts.LocalServingPort)

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return ctx, c
}

// registerEveryWebhookTheManifestDeclares registers a handler for every path in the configuration envtest
// installs, and refuses to run if the manifest names one this file does not know about.
//
// It used to register the MLTrainingJob validator and nothing else, while the manifest beside it declared
// three webhooks with failurePolicy: Fail. So the apiserver called /validate-batch-v1-job on a manager that
// did not serve it, got "the server could not find the requested resource", and refused the write -- which
// is indistinguishable from the refusal a spec is trying to observe, and is why this suite was red.
//
// Driving registration FROM the manifest rather than beside it is the point. A webhook added to the
// configuration and not here now stops the suite with a message naming the path, instead of turning every
// governed write in the test cluster into a 404.
func registerEveryWebhookTheManifestDeclares(t *testing.T, mgr manager.Manager) {
	t.Helper()

	setups := map[string]func(manager.Manager) error{
		"/validate-platform-lkhun9311-github-io-v1-mltrainingjob": SetupMLTrainingJobWebhookWithManager,
		"/validate--v1-pod":      SetupGPUPodWebhookWithManager,
		"/validate-batch-v1-job": SetupGPUJobWebhookWithManager,
	}

	path := filepath.Join("..", "..", "..", "config", "webhook", "manifests.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg admissionv1.ValidatingWebhookConfiguration
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(cfg.Webhooks) == 0 {
		t.Fatalf("%s declares no webhooks; this harness would serve an apiserver that consults nothing", path)
	}

	for _, w := range cfg.Webhooks {
		if w.ClientConfig.Service == nil || w.ClientConfig.Service.Path == nil {
			t.Fatalf("webhook %q names no handler path", w.Name)
		}
		p := *w.ClientConfig.Service.Path
		setup, ok := setups[p]
		if !ok {
			t.Fatalf("the installed configuration declares webhook %q at %q and this harness has no setup "+
				"for it; the apiserver would call a path this manager does not serve, and failurePolicy %v "+
				"turns that into a refusal no spec can tell from a real one",
				w.Name, p, policyOf(w.FailurePolicy))
		}
		if err := setup(mgr); err != nil {
			t.Fatalf("register %s: %v", p, err)
		}
	}
}

// policyOf renders a failure policy for a message, since it is a pointer and the nil case means "default".
func policyOf(p *admissionv1.FailurePolicyType) string {
	if p == nil {
		return "Fail (default)"
	}
	return string(*p)
}

// waitForWebhookServer blocks until the TLS listener accepts, so a refusal cannot be mistaken for a race.
//
// Without this the first write could fail because the server had not bound yet, and failurePolicy: Fail
// would turn that into a rejection indistinguishable from the one the test is trying to observe.
func waitForWebhookServer(t *testing.T, host string, port int) {
	t.Helper()
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the cert is envtest's own, generated for this process
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook server never accepted on %s: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// firstEnvtestBinaryDir mirrors the controller package's lookup so this suite runs from an IDE too.
func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := filepath.Glob(filepath.Join(base, "*"))
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0]
}

func mltj(name string, mutate func(*platformv1.MLTrainingJob)) *platformv1.MLTrainingJob {
	j := &platformv1.MLTrainingJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: platformv1.MLTrainingJobSpec{
			Queue: "team-a", Image: "trainer:v1", GPUCount: 1, Parallelism: 1, Completions: 1,
		},
	}
	if mutate != nil {
		mutate(j)
	}
	return j
}

func TestApiserverRefusesABlankImage(t *testing.T) {
	// The controller suite asserts the apiserver ACCEPTS this when no webhook is installed. Here the only
	// difference is the webhook, so a rejection is attributable to it and to nothing else.
	ctx, c := startAdmissionEnv(t)

	err := c.Create(ctx, mltj("blank-image", func(j *platformv1.MLTrainingJob) { j.Spec.Image = "" }))
	if err == nil {
		t.Fatal("apiserver accepted a blank image; the webhook was not consulted")
	}
	if !strings.Contains(err.Error(), "spec.image") {
		t.Fatalf("rejected for something other than the image: %v", err)
	}
}

func TestApiserverAcceptsARunnableSpec(t *testing.T) {
	// The control. Without it, a webhook that rejected everything — or a manifest whose rules matched the
	// wrong resource and errored on every write — would pass the rejection test above.
	ctx, c := startAdmissionEnv(t)

	if err := c.Create(ctx, mltj("runnable", nil)); err != nil {
		t.Fatalf("apiserver refused a valid MLTrainingJob: %v", err)
	}
}

func TestApiserverRefusesAnImageEditOnceTheJobExists(t *testing.T) {
	// The whole point of the webhook, exercised through the apiserver: this same edit is stored and silently
	// ignored when nothing is validating it.
	ctx, c := startAdmissionEnv(t)

	job := mltj("immutable", nil)
	if err := c.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Editing is still permitted while no Job exists, which is itself part of the contract.
	job.Spec.Image = "trainer:v2"
	if err := c.Update(ctx, job); err != nil {
		t.Fatalf("refused an edit made before the Job existed: %v", err)
	}

	// Stand in for the reconciler by creating the owned Job it would create.
	if err := c.Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace},
		Spec: batchv1.JobSpec{
			Template: minimalPodTemplate(),
		},
	}); err != nil {
		t.Fatalf("create owned job: %v", err)
	}

	var fresh platformv1.MLTrainingJob
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), &fresh); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	fresh.Spec.Image = "trainer:v3"
	err := c.Update(ctx, &fresh)
	if err == nil {
		t.Fatal("apiserver stored an image edit the running Job would never pick up")
	}
	if !strings.Contains(err.Error(), "spec.image") {
		t.Fatalf("rejected for something other than the image: %v", err)
	}
}

func TestApiserverAllowsDeleteWithTheWebhookInstalled(t *testing.T) {
	// DELETE is left out of the manifest's operations on purpose, and this is what holds that still: a
	// webhook able to refuse a delete could strand an object nobody can remove.
	ctx, c := startAdmissionEnv(t)

	job := mltj("deletable", nil)
	if err := c.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Delete(ctx, job); err != nil {
		t.Fatalf("refused a delete: %v", err)
	}
}

// minimalPodTemplate is the smallest pod template the Job schema accepts.
func minimalPodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "trainer", Image: "trainer:v1"}},
		},
	}
}

// The same guarantee through a real apiserver, because this one is about a path the unit test cannot walk:
// the object has to actually be deleting, with a finalizer holding it, and the update has to be the one that
// lets go.
//
// A webhook that refuses this update does not fail loudly. The delete call SUCCEEDS — it only sets a
// deletion timestamp — and the object then sits in Terminating with no error anywhere until someone notices
// it never went away.
func TestApiserverLetsADeletingObjectFinish(t *testing.T) {
	ctx, c := startAdmissionEnv(t)

	// Created valid, because the webhook is installed and would refuse an empty image outright. What is
	// simulated is a spec that BECAME unacceptable — the shape a legacy object has after a rule is added.
	obj := mltj("deleting", func(j *platformv1.MLTrainingJob) {
		j.Finalizers = []string{"platform.lkhun9311.github.io/mltrainingjob"}
	})
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := c.Delete(ctx, obj); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var deleting platformv1.MLTrainingJob
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &deleting); err != nil {
		t.Fatalf("the finalizer should be holding the object: %v", err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("the object is not actually deleting, so this test proves nothing")
	}

	// Now it fails the create-time rules, the way an object predating them does.
	deleting.Spec.Image = ""
	deleting.Finalizers = nil
	if err := c.Update(ctx, &deleting); err != nil {
		t.Fatalf("the apiserver refused the update that removes the finalizer, so the object is stranded: %v", err)
	}

	var gone platformv1.MLTrainingJob
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), &gone)
	if err == nil {
		t.Fatal("the object survived its own deletion")
	}
}
