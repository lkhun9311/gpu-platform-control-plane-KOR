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
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ErrKeyStoreUnavailable reports that the api-keys Secret could not be read, which is a different fact from
// a key that is not in it.
//
// It exists because the two used to collapse into one boolean. A Secret the gateway could not read — RBAC
// revoked, the object deleted, the apiserver briefly unreachable — answered every request 401, so a caller
// with a perfectly valid key was told their credential was wrong. That sends an operator to rotate keys
// while the actual fault is on the cluster, and it does so uniformly for every tenant at once, which is the
// shape of an outage rather than of an auth failure.
var ErrKeyStoreUnavailable = errors.New("api-keys secret could not be read")

// resolveTenant maps the request's Bearer API key to a tenant via the api-keys Secret.
//
// It returns ok=false for a missing or unknown key, and ErrKeyStoreUnavailable when the answer could not be
// established at all.
func (s *Server) resolveTenant(ctx context.Context, r *http.Request) (string, bool, error) {
	h := r.Header.Get("Authorization")
	// Split the header into scheme and credential on the first space.
	scheme, credential, found := strings.Cut(h, " ")
	// HTTP auth schemes are case-insensitive per RFC 7235, so compare with EqualFold.
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false, nil
	}
	key := strings.TrimSpace(credential)
	if key == "" {
		return "", false, nil
	}
	var sec corev1.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.APIKeySecret, Namespace: s.Namespace}, &sec); err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrKeyStoreUnavailable, err)
	}
	tenant, ok := sec.Data[key]
	return string(tenant), ok && len(tenant) > 0, nil
}
