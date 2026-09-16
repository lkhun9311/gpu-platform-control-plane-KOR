package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// The scale benchmarks measure the CACHED client, because that is the one production runs and the one the
// rest of this package never exercises.
//
// The Ginkgo suite builds its client with client.New, which reads straight through to the apiserver. Every
// cache-shaped property is therefore invisible to it: whether a List consults an index or scans the store,
// whether a label selector narrows anything, how any of it grows with the object count. Those are exactly
// the properties a scale claim is about, so measuring them requires standing up a real informer cache
// rather than reusing the suite's.
//
// Each benchmark starts its own envtest because BeforeSuite only runs inside RunSpecs, and a benchmark is
// not a spec.

// startCachedClient brings up an apiserver and a started informer cache, and returns a reader backed by it.
//
// The writer is uncached: fixtures must land in the apiserver for the cache to observe them, and writing
// through the cache's reader would be a different code path than the one under measurement.
func startCachedClient(b testing.TB) (context.Context, client.Client, client.Client) {
	b.Helper()

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "kueue-crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}

	cfg, err := env.Start()
	if err != nil {
		b.Skipf("envtest unavailable, skipping scale benchmark: %v", err)
	}
	b.Cleanup(func() { _ = env.Stop() })

	if err := platformv1.AddToScheme(scheme.Scheme); err != nil {
		b.Fatalf("add platform scheme: %v", err)
	}
	if err := kueuev1beta1.AddToScheme(scheme.Scheme); err != nil {
		b.Fatalf("add kueue scheme: %v", err)
	}

	writer, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		b.Fatalf("build writer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	reader := startCache(b, ctx, cfg)
	return ctx, writer, reader
}

// startCache builds the informer cache and blocks until it has synced.
func startCache(b testing.TB, ctx context.Context, cfg *rest.Config) client.Client {
	b.Helper()

	c, err := cache.New(cfg, cache.Options{Scheme: scheme.Scheme})
	if err != nil {
		b.Fatalf("build cache: %v", err)
	}
	// The same index SetupWithManager installs. Without it the benchmark would measure a lookup production
	// never performs, and the MatchingFields List would fail rather than scan.
	if err := c.IndexField(ctx, &kueuev1beta1.Workload{}, WorkloadJobRefIndex, indexWorkloadByJobRef); err != nil {
		b.Fatalf("index workloads: %v", err)
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		b.Fatal("cache never synced")
	}

	reader, err := client.New(cfg, client.Options{Scheme: scheme.Scheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		b.Fatalf("build cached reader: %v", err)
	}
	return reader
}

// seedWorkloads creates n Kueue Workloads carrying a job-uid label that matches no Job under measurement.
//
// The label is present and distinct on every fixture rather than absent, so the benchmark measures a
// selector that has to reject n candidates rather than one the store can dismiss for lacking the key.
func seedWorkloads(b testing.TB, ctx context.Context, writer, reader client.Client, ns string, n int) {
	b.Helper()

	if err := writer.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		b.Fatalf("create namespace: %v", err)
	}

	for i := range n {
		wl := &kueuev1beta1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("wl-%d", i),
				Namespace: ns,
				Labels:    map[string]string{kueueJobUIDLabel: fmt.Sprintf("uid-%d", i)},
			},
			Spec: kueuev1beta1.WorkloadSpec{
				QueueName: "bench",
				PodSets: []kueuev1beta1.PodSet{{
					Name:  "main",
					Count: 1,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "main", Image: "busybox"}},
						},
					},
				}},
			},
		}
		if err := writer.Create(ctx, wl); err != nil {
			b.Fatalf("create workload %d: %v", i, err)
		}
	}

	// The cache is eventually consistent with the writes above, and a benchmark that started before it caught
	// up would time a store holding fewer objects than the size it reports.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var list kueuev1beta1.WorkloadList
		if err := reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
			b.Fatalf("list during sync wait: %v", err)
		}
		if len(list.Items) >= n {
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("cache saw %d of %d workloads before the deadline", len(list.Items), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// BenchmarkGetWorkloadForJobMiss measures the lookup on the path that misses the label.
//
// The miss is the path worth measuring because it is the COMMON one, not an edge: every MLTrainingJob
// reconciles at least once before Kueue has created its Workload, so every job takes this path on the way in.
// On a miss the function does not stop at the label List — it falls back to listing every Workload in the
// namespace and walking their owner references, so the cost per reconcile is a function of how many jobs the
// namespace already holds.
func BenchmarkGetWorkloadForJobMiss(b *testing.B) {
	for _, n := range []int{100, 500, 2000} {
		b.Run(fmt.Sprintf("workloads=%d", n), func(b *testing.B) {
			ctx, writer, reader := startCachedClient(b)
			ns := fmt.Sprintf("bench-miss-%d", n)
			seedWorkloads(b, ctx, writer, reader, ns, n)

			r := &MLTrainingJobReconciler{Client: reader, Scheme: scheme.Scheme}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: ns, UID: types.UID("uid-absent")},
			}

			b.ResetTimer()
			for range b.N {
				wl, err := r.getWorkloadForJob(ctx, job)
				if err != nil {
					b.Fatalf("lookup: %v", err)
				}
				if wl != nil {
					b.Fatal("fixture is wrong: a workload matched a job that owns none")
				}
			}
		})
	}
}

// BenchmarkGetWorkloadForJobHit measures the lookup once Kueue has created the Workload.
//
// Its value is as a CONTROL for the miss benchmark. If both grow with n, the fallback is not what costs —
// the label List itself scans. If only the miss grows, the scan is the fallback's, and the two have different
// fixes. Reading the miss number alone cannot tell those apart.
func BenchmarkGetWorkloadForJobHit(b *testing.B) {
	for _, n := range []int{100, 500, 2000} {
		b.Run(fmt.Sprintf("workloads=%d", n), func(b *testing.B) {
			ctx, writer, reader := startCachedClient(b)
			ns := fmt.Sprintf("bench-hit-%d", n)
			seedWorkloads(b, ctx, writer, reader, ns, n)

			r := &MLTrainingJobReconciler{Client: reader, Scheme: scheme.Scheme}
			// The last seeded Workload, so a store that scans in insertion order cannot look fast by finding it early.
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name: "present", Namespace: ns, UID: types.UID(fmt.Sprintf("uid-%d", n-1)),
				},
			}

			b.ResetTimer()
			for range b.N {
				wl, err := r.getWorkloadForJob(ctx, job)
				if err != nil {
					b.Fatalf("lookup: %v", err)
				}
				if wl == nil {
					b.Fatal("fixture is wrong: the seeded workload was not found")
				}
			}
		})
	}
}
