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

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// ErrNoRoute is the sentinel reported when no InferenceDeployment serves the requested model.
//
// Design rationale (design spec Error codes section): an unknown model must get a 404.
//
// This signals "no such model" rather than a read failure, so the caller translates it to 404.
var ErrNoRoute = errors.New("no inferencedeployment for model")

// BackendRef identifies the resolved serving backend, not just its URL.
//
// A later scraper task needs the namespace, name, and port to build the backend's metrics URL and manage its telemetry lifecycle, which a bare *url.URL cannot provide.
type BackendRef struct {
	// Namespace and Name locate the InferenceDeployment, and its same-named Service, that serves the request.
	Namespace string
	Name      string
	// Port is the Service port the InferenceDeployment serves on.
	Port int32
	// URL is the in-cluster address the proxy forwards the request to.
	URL *url.URL
	// Model is the requested model name, carried through for metric labels.
	Model string
}

// servingPort returns the port the InferenceDeployment serves on.
//
// spec.port carries +kubebuilder:default=8080, so anything that went through the API server already has it set and zero cannot occur in production.
//
// The fake client used in tests applies no defaulting, though, and a URL built on port 0 is unroutable, hence the same fallback here.
func servingPort(infd *platformv1.InferenceDeployment) int32 {
	if infd.Spec.Port == 0 {
		return 8080
	}
	return infd.Spec.Port
}

// olderInfD reports whether a takes precedence over b.
//
// The earlier creationTimestamp wins, and on an exact tie the lexicographically smaller name wins.
//
// The name tie-break is what makes the order total: creationTimestamp has second granularity, so two objects created in the same second compare equal and a timestamp-only rule leaves them unordered.
//
// Names are unique within a namespace, so comparing them always decides.
//
// olderPolicy in ratelimit.go applies the same rule to GPUQuotaPolicy.
func olderInfD(a, b *platformv1.InferenceDeployment) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		// Same timestamp, so tie-break on ascending name.
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// backendsFor resolves the requested model to every InferenceDeployment serving it, ordered head first, or ErrNoRoute if none does.
//
// Design rationale (design spec Components section): the lookup goes through the ModelNameIndex field index on the cache, so it needs no CR field selector and no per-request apiserver call.
//
// Scoping the lookup to the policy's TargetNamespace is what isolates tenants: distinct tenants commonly serve the same model name, so dropping the namespace would leak requests across them.
//
// Design rationale (design spec Identity model section): nothing stops two InferenceDeployments from serving the same model.
//
// Requests must not bounce between backends in that case, so exactly one is chosen by a deterministic rule (oldest wins, ties break on ascending name) and the duplicate is logged.
// backendsFor returns every backend serving the model, ordered, head first.
func (s *Server) backendsFor(ctx context.Context, policy *platformv1.GPUQuotaPolicy, model string) ([]*BackendRef, error) {
	var list platformv1.InferenceDeploymentList
	if err := s.Client.List(ctx, &list,
		client.InNamespace(policy.Spec.TargetNamespace),
		client.MatchingFields{ModelNameIndex: model}); err != nil {
		return nil, fmt.Errorf("list inferencedeployments: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, ErrNoRoute
	}

	// Sorted oldest-first, and the ORDER is the routing policy rather than a tidiness detail.
	//
	// A second deployment serving the same model used to be logged as an operator problem and then discarded.
	// It is the ordinary shape of a served model — a rollout, a second replica set, a spare — and discarding it
	// meant a request failed whenever the one chosen backend was down, while a healthy alternative sat one
	// list entry away. The head stays exactly what it was, so a single-backend model routes identically and no
	// existing behaviour moves; the tail is what fallback walks.
	//
	// Oldest-first rather than newest, because a request must resolve deterministically across gateway
	// replicas: two gateways picking different heads for the same model would make latency depend on which
	// one a client happened to reach.
	sorted := make([]*platformv1.InferenceDeployment, 0, len(list.Items))
	for i := range list.Items {
		sorted = append(sorted, &list.Items[i])
	}
	slices.SortFunc(sorted, func(a, b *platformv1.InferenceDeployment) int {
		switch {
		case olderInfD(a, b):
			return -1
		case olderInfD(b, a):
			return 1
		default:
			return 0
		}
	})
	refs := make([]*BackendRef, 0, len(sorted))
	for _, d := range sorted {
		port := servingPort(d)
		// The controller names the Service after the InferenceDeployment in the same namespace, so the in-cluster address is http://<name>.<namespace>.svc:<port>.
		u, err := url.Parse(fmt.Sprintf("http://%s.%s.svc:%d", d.Name, d.Namespace, port))
		if err != nil {
			return nil, fmt.Errorf("parse backend url: %w", err)
		}
		refs = append(refs, &BackendRef{Namespace: d.Namespace, Name: d.Name, Port: port, URL: u, Model: model})
	}
	return refs, nil
}
