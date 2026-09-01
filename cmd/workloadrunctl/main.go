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

// Command workloadrunctl runs the WorkloadRun reconciler and nothing else.
//
// The manager binary runs every controller, which is what a cluster wants and exactly what an evidence run
// must not do: pointing it at a cluster that already has an operator puts two reconcilers on the same
// objects. This exists so a WorkloadRun can be driven against a real apiserver without that, and so the
// end-to-end script can prove the trail on a throwaway cluster rather than in envtest.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/controller"
)

func main() {
	var (
		name      string
		namespace string
		timeout   time.Duration
	)
	flag.StringVar(&name, "name", "", "the WorkloadRun to drive")
	flag.StringVar(&namespace, "namespace", "default", "its namespace")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "give up if the run has not reached a terminal phase")
	var poll time.Duration
	flag.DurationVar(&poll, "poll", 0, "how often to look at the open window; zero uses the controller's default")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	if name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register client-go scheme: %v\n", err)
		os.Exit(1)
	}
	if err := platformv1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register platform scheme: %v\n", err)
		os.Exit(1)
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}

	r := &controller.WorkloadRunReconciler{Client: c, Scheme: scheme, Poll: poll}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// A plain loop rather than a manager. The reconciler asks to be requeued and this obeys that, so the
	// poll cadence -- which the gap check measures against -- is the controller's own rather than this
	// wrapper's idea of one.
	req := ctrl.Request{}
	req.Name, req.Namespace = name, namespace
	for {
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
			os.Exit(1)
		}
		var run platformv1.WorkloadRun
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
			fmt.Fprintf(os.Stderr, "read back: %v\n", err)
			os.Exit(1)
		}
		switch run.Status.Phase {
		case platformv1.WorkloadRunComplete:
			fmt.Printf("COMPLETE verdict=%s reason=%s\n", run.Status.Verdict, run.Status.Reason)
			return
		case platformv1.WorkloadRunRefused:
			// Exit 0. A refusal is a correct outcome of a correct controller -- it is the trail saying it
			// cannot stand behind a verdict -- and a non-zero exit would make the driving script treat the
			// discipline as a malfunction.
			fmt.Printf("REFUSED reason=%s\n", run.Status.Reason)
			return
		}
		if res.RequeueAfter <= 0 {
			res.RequeueAfter = time.Second
		}
		select {
		case <-time.After(res.RequeueAfter):
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "timed out with the run still in phase %q\n", run.Status.Phase)
			os.Exit(1)
		}
	}
}
