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

// Command queueload fills a namespace with MLTrainingJobs and watches the operator's workqueue drain.
//
// It exists because the previous attempt at this measurement did not measure it. That harness created
// objects with `kubectl apply` in batches and reached 75 objects per second, while the operator absorbed
// 143 reconciles per second — so the queue never had anything in it and the recorded peak depth was zero.
// A load test whose load is below the system's service rate measures the load generator.
//
// Two things are different here.
//
// The client is concurrent and in-process: no per-object process start, no repeated TLS handshake, one
// client reused across workers. That is what makes it possible to offer work faster than the operator
// retires it.
//
// The queue depth is read from the operator's own /metrics endpoint at an interval the caller sets, rather
// than from Prometheus. Prometheus scrapes on its own schedule — 15s here — so sampling it every 5s reports
// the same scraped value three times and cannot see a spike that opens and closes between scrapes. Reading
// the endpoint directly makes the sampling interval the caller's to choose, and the harness records what it
// actually achieved so a peak is never quoted at a resolution it did not have.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var scheme = runtime.NewScheme()

// bearer authenticates to the metrics endpoint, and metricsClient skips certificate verification.
//
// Both are deliberate and both are why this tool is a harness rather than a component. The operator serves
// metrics over TLS with a self-signed certificate and requires a token bound to the metrics-reader role, so
// a measurement client either carries the CA and a token or it reads nothing. Skipping verification is
// acceptable here because the endpoint is a localhost port-forward the operator of this harness created; it
// would not be acceptable anywhere that decision was not the caller's.
var (
	bearer        string
	metricsClient = &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // localhost port-forward, see above
		},
	}
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(platformv1.AddToScheme(scheme))
}

// sample is one reading of the operator's own counters.
type sample struct {
	AtMs  int64 `json:"atMs"`
	Depth int   `json:"depth"`
	Adds  int   `json:"adds"`
	Done  int   `json:"reconciles"`
}

// record is what this run leaves behind.
type record struct {
	Objects            int      `json:"objects"`
	Concurrency        int      `json:"concurrency"`
	CreateMs           float64  `json:"createMs"`
	CreatesPerSec      float64  `json:"createsPerSec"`
	DrainMs            *float64 `json:"drainMs"`
	Reconciles         int      `json:"reconciles"`
	ReconcilesPerSec   *float64 `json:"reconcilesPerSec"`
	PeakDepth          int      `json:"peakQueueDepth"`
	SampleIntervalMs   float64  `json:"requestedSampleIntervalMs"`
	AchievedIntervalMs float64  `json:"achievedSampleIntervalMs"`
	Samples            []sample `json:"samples"`
	Verdict            string   `json:"verdict"`
	Caveat             string   `json:"caveat"`
}

func main() {
	var (
		ns          = flag.String("namespace", "queue-load", "namespace to fill")
		count       = flag.Int("count", 600, "MLTrainingJobs to create")
		concurrency = flag.Int("concurrency", 32, "concurrent creators")
		metricsURL  = flag.String("metrics", "https://localhost:18443/metrics", "the operator's metrics endpoint")
		tokenFile   = flag.String("token-file", "", "bearer token for the metrics endpoint; controller-runtime serves it over TLS behind authn/authz, so a plain GET is 401")
		sampleEvery = flag.Duration("sample", 200*time.Millisecond, "how often to read the metrics endpoint")
		out         = flag.String("out", "./ex/queue-load.json", "where to write the record")
		queue       = flag.String("queue", "team-a", "spec.queue on each job")
	)
	flag.Parse()

	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ABORT: read token: %v\n", err)
			os.Exit(1)
		}
		bearer = strings.TrimSpace(string(b))
	}
	if err := run(*ns, *queue, *metricsURL, *out, *count, *concurrency, *sampleEvery); err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: %v\n", err)
		os.Exit(1)
	}
}

func run(ns, queue, metricsURL, out string, count, concurrency int, sampleEvery time.Duration) error {
	ctx := context.Background()
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	// The steady state is checked before anything is created, because a queue that is already backed up makes
	// the drain time meaningless and a peak that includes somebody else's work is not this run's peak.
	before, err := readMetrics(metricsURL)
	if err != nil {
		return fmt.Errorf("read %s: %w", metricsURL, err)
	}
	if before.Depth != 0 {
		return fmt.Errorf("the workqueue already holds %d items; this run could not attribute a peak to itself",
			before.Depth)
	}
	fmt.Printf("  steady state: depth=%d reconciles=%d\n", before.Depth, before.Done)

	if err := ensureNamespace(ctx, c, ns); err != nil {
		return err
	}
	defer func() {
		_ = c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		fmt.Printf("\n  removing the load\n")
	}()

	// Sampling starts BEFORE the first create, so the rise is captured and not only the fall.
	stop := make(chan struct{})
	var samples []sample
	var peak int
	var achieved float64
	var wgSample sync.WaitGroup
	wgSample.Add(1)
	t0 := time.Now()
	go func() {
		defer wgSample.Done()
		var n int
		last := time.Now()
		var total time.Duration
		for {
			select {
			case <-stop:
				if n > 1 {
					achieved = float64(total.Milliseconds()) / float64(n-1)
				}
				return
			default:
			}
			m, err := readMetrics(metricsURL)
			if err == nil {
				s := sample{AtMs: time.Since(t0).Milliseconds(), Depth: m.Depth, Adds: m.Adds, Done: m.Done}
				samples = append(samples, s)
				if m.Depth > peak {
					peak = m.Depth
				}
			}
			now := time.Now()
			if n > 0 {
				total += now.Sub(last)
			}
			last = now
			n++
			time.Sleep(sampleEvery)
		}
	}()

	// Create concurrently. This is the whole difference from the previous harness.
	fmt.Printf("  creating %d MLTrainingJobs with %d concurrent writers\n", count, concurrency)
	createStart := time.Now()
	var created, failed atomic.Int64
	work := make(chan int, count)
	for i := 1; i <= count; i++ {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for i := range work {
				job := &platformv1.MLTrainingJob{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("load-%d", i), Namespace: ns},
					Spec: platformv1.MLTrainingJobSpec{
						Queue: queue, Image: "busybox:1.36", Command: []string{"sh", "-c", "sleep 1"},
						GPUCount: 1, Parallelism: 1, Completions: 1,
					},
				}
				if err := c.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
					failed.Add(1)
					continue
				}
				created.Add(1)
			}
		})
	}
	wg.Wait()
	createMs := float64(time.Since(createStart).Microseconds()) / 1000.0
	fmt.Printf("  created %d (failed %d) in %.1fms = %.1f/sec\n",
		created.Load(), failed.Load(), createMs, float64(created.Load())/(createMs/1000.0))

	// Wait for the queue to empty, requiring several consecutive empty reads: one can land between two bursts
	// of requeues and call a queue drained that is not.
	drainStart := time.Now()
	var drainMs *float64
	empty := 0
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		m, err := readMetrics(metricsURL)
		if err == nil {
			if m.Depth <= 0 {
				empty++
				if empty >= 5 {
					d := float64(time.Since(drainStart).Microseconds()) / 1000.0
					drainMs = &d
					break
				}
			} else {
				empty = 0
			}
		}
		time.Sleep(sampleEvery)
	}
	close(stop)
	wgSample.Wait()

	after, err := readMetrics(metricsURL)
	if err != nil {
		return fmt.Errorf("read %s after the drain: %w", metricsURL, err)
	}
	reconciles := after.Done - before.Done

	rec := record{
		Objects: int(created.Load()), Concurrency: concurrency,
		CreateMs: createMs, CreatesPerSec: float64(created.Load()) / (createMs / 1000.0),
		DrainMs: drainMs, Reconciles: reconciles, PeakDepth: peak,
		SampleIntervalMs: float64(sampleEvery.Microseconds()) / 1000.0, AchievedIntervalMs: achieved,
		Samples: samples,
	}
	if drainMs != nil && *drainMs > 0 {
		r := float64(reconciles) / (*drainMs / 1000.0)
		rec.ReconcilesPerSec = &r
	}
	switch peak {
	case 0:
		rec.Verdict = "the queue never held anything: this run measured the client, not the operator"
	default:
		rec.Verdict = fmt.Sprintf("the queue backed up to %d items, so the operator was the bottleneck", peak)
	}
	rec.Caveat = fmt.Sprintf(
		"peak depth is a lower bound at a %.0fms achieved sampling interval; a spike opening and closing "+
			"between two reads is invisible", achieved)

	fmt.Printf("\n  peak queue depth   %d\n  drain              %s\n  reconciles         %d\n  sampling           %.0fms achieved\n  %s\n",
		peak, msOrNever(drainMs), reconciles, achieved, rec.Verdict)

	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Printf("  record written to %s\n", out)
	return nil
}

func msOrNever(v *float64) string {
	if v == nil {
		return "never emptied"
	}
	return fmt.Sprintf("%.1fms", *v)
}

func ensureNamespace(ctx context.Context, c client.Client, ns string) error {
	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return nil
}

// metrics is the subset of the operator's exposition this harness reads.
type metrics struct {
	Depth int
	Adds  int
	Done  int
}

// readMetrics parses controller-runtime's workqueue series for the mltrainingjob controller.
//
// Parsed by hand rather than with a Prometheus client library because the three series wanted are simple
// gauges and counters, and the alternative is a dependency that would arrive with its own registry.
func readMetrics(url string) (metrics, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return metrics{}, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := metricsClient.Do(req)
	if err != nil {
		return metrics{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return metrics{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var m metrics
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		// The two families label the same controller differently: workqueue series carry name=, and
		// controller-runtime's reconcile counter carries controller=. The first version of this filter
		// required name= on all three and reported zero reconciles while draining six hundred items — a
		// number obviously wrong, which is the only reason it was caught before being written down.
		switch {
		case strings.HasPrefix(line, "workqueue_depth") && strings.Contains(line, `name="mltrainingjob"`):
			m.Depth += intValue(line)
		case strings.HasPrefix(line, "workqueue_adds_total") && strings.Contains(line, `name="mltrainingjob"`):
			m.Adds += intValue(line)
		case strings.HasPrefix(line, "controller_runtime_reconcile_total") &&
			strings.Contains(line, `controller="mltrainingjob"`):
			m.Done += intValue(line)
		}
	}
	return m, sc.Err()
}

func intValue(line string) int {
	i := strings.LastIndex(line, " ")
	if i < 0 {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
	if err != nil {
		return 0
	}
	return int(f)
}
