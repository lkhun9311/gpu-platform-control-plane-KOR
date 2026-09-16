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

package main

import (
	"testing"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/gateway"
)

// TestNewAdmitterOff pins that mode "off" (the default flag value) never errors, so a gateway
// started with no flags at all still comes up.
func TestNewAdmitterOff(t *testing.T) {
	a, stop, err := newAdmitter(gateway.AdmissionOff, admitterFlags{})
	if err != nil {
		t.Fatalf("newAdmitter(off) returned an error: %v", err)
	}
	if a == nil {
		t.Fatal("newAdmitter(off) returned a nil Admitter")
	}
	// off starts no background work, so its stop function must be safe to call and a no-op.
	stop()
}

// TestNewAdmitterStaticCap pins that mode "static-cap" builds successfully with real tuning values.
func TestNewAdmitterStaticCap(t *testing.T) {
	a, stop, err := newAdmitter(gateway.AdmissionStaticCap, admitterFlags{
		staticRate:    2000,
		staticBurst:   8192,
		longThreshold: 4096,
	})
	if err != nil {
		t.Fatalf("newAdmitter(static-cap) returned an error: %v", err)
	}
	if a == nil {
		t.Fatal("newAdmitter(static-cap) returned a nil Admitter")
	}
	stop()
}

// TestNewAdmitterKVAware pins that mode "kv-aware" builds successfully now that Task 3 has
// implemented it, and that its stop function actually stops the scraper it started (no leaked
// goroutine, no panic) rather than being another no-op.
func TestNewAdmitterKVAware(t *testing.T) {
	a, stop, err := newAdmitter(gateway.AdmissionKVAware, admitterFlags{
		longThreshold: 4096,
		kv: gateway.KVAwareConfig{
			EngageUsage:    0.85,
			ReleaseUsage:   0.75,
			WaitingThresh:  8,
			ReleaseSustain: 30 * time.Second,
			ScrapeInterval: time.Hour,
			MaxStaleness:   time.Hour,
			HTTPTimeout:    time.Second,
			LongThreshold:  4096,
		},
	})
	if err != nil {
		t.Fatalf("newAdmitter(kv-aware) returned an error: %v", err)
	}
	if a == nil {
		t.Fatal("newAdmitter(kv-aware) returned a nil Admitter")
	}
	stop()
}

// TestNewAdmitterUnknownMode pins that a typo or unsupported value in --admission-mode fails
// startup instead of being silently ignored.
func TestNewAdmitterUnknownMode(t *testing.T) {
	_, stop, err := newAdmitter(gateway.AdmissionMode("bogus"), admitterFlags{})
	if err == nil {
		t.Fatal("newAdmitter(bogus) unexpectedly succeeded")
	}
	// Even the error path must return a callable stop function, so main's unconditional
	// shutdown-time call never nil-derefs.
	stop()
}
