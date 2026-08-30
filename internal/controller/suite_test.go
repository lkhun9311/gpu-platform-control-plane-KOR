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
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework).
//
// Refer to http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	// cachedClient is what the MLTrainingJob reconciler gets, because its Workload lookup resolves through a
	// FIELD INDEX and an index only exists on a cache — a client reading through to the apiserver would have
	// its field selector rejected, since the apiserver defines no such field for the resource.
	//
	// Only Workloads are served from the cache. Everything else keeps reading through, so the specs' pattern
	// of writing an object and reconciling on the next line still observes what it just wrote. That pattern is
	// a convenience of the harness rather than a property of production, and narrowing the cache to the one
	// type that needs it buys the index without rewriting every spec around cache lag.
	cachedClient  client.Client
	workloadCache cache.Cache
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = platformv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	Expect(kueuev1beta1.AddToScheme(scheme.Scheme)).To(Succeed())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crd", "kueue"),
		},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	workloadCache, err = cache.New(cfg, cache.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(workloadCache.IndexField(
		ctx, &kueuev1beta1.Workload{}, WorkloadJobRefIndex, indexWorkloadByJobRef,
	)).To(Succeed())
	go func() {
		defer GinkgoRecover()
		Expect(workloadCache.Start(ctx)).To(Succeed())
	}()
	Expect(workloadCache.WaitForCacheSync(ctx)).To(BeTrue())

	cachedClient, err = client.New(cfg, client.Options{
		Scheme: scheme.Scheme,
		Cache: &client.CacheOptions{
			Reader: workloadCache,
			// Reads of these types stay direct, so a spec that creates one and reconciles immediately still sees it.
			DisableFor: []client.Object{
				&platformv1.MLTrainingJob{}, &batchv1.Job{}, &corev1.Pod{}, &corev1.Node{},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
//
// ENVTEST-based tests depend on specific binaries, usually located in paths set by controller-runtime.
//
// When running tests directly (e.g., via an IDE) without using Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to setting the 'KUBEBUILDER_ASSETS' environment variable.
//
// To ensure the binaries are properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// awaitCachedWorkload blocks until the reconciler's cache has observed a Workload satisfying pred.
//
// The specs write Workloads through the direct client and then reconcile on the next line. That ordering is
// only safe against a client that reads through; the reconciler reads Workloads from a cache, which is
// eventually consistent with those writes. Waiting here makes the lag explicit instead of leaving the specs
// to pass or fail on how quickly the informer happened to catch up.
func awaitCachedWorkload(name, namespace string, pred func(*kueuev1beta1.Workload) bool) {
	GinkgoHelper()
	Eventually(func() bool {
		var wl kueuev1beta1.Workload
		if err := cachedClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &wl); err != nil {
			return false
		}
		return pred(&wl)
	}, 10*time.Second, 50*time.Millisecond).Should(BeTrue(), "cache never observed workload %s/%s", namespace, name)
}

// hasAdmittedCondition reports whether a Workload carries an Admitted=True condition.
//
// Named for the condition rather than the state because workloadAdmitted is already a fixture BUILDER in
// this package, and a predicate sharing that name would read as its inverse.
func hasAdmittedCondition(wl *kueuev1beta1.Workload) bool {
	for _, c := range wl.Status.Conditions {
		if c.Type == "Admitted" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
