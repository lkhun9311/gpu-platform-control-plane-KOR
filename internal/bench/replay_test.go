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

package bench

import (
	"bytes"
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// slowSender blocks for delay before returning, modelling a saturated backend whose responses hang.
type slowSender struct {
	delay time.Duration
}

func (s slowSender) Send(ctx context.Context, row TraceRow, sendUnixNanos int64) SendResult {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	end := time.Now().UnixNano()
	return SendResult{FirstTokenUnixNanos: sendUnixNanos + int64(time.Millisecond), EndUnixNanos: end, OutputTokens: 8, HTTPStatus: 200}
}

// evenTrace builds a trace whose arrivals are spaced gapMs apart, so a test can assert the sender fires on that schedule.
func evenTrace(n int, gapMs int64) []TraceRow {
	rows := make([]TraceRow, n)
	for i := range rows {
		rows[i] = TraceRow{Index: i, OffsetMs: int64(i) * gapMs, Tenant: "premium-1", PromptLenChars: 200, MaxOutputTokens: 16}
	}
	return rows
}

var _ = Describe("Replay open-loop scheduling", func() {
	It("does not let a hung backend delay subsequent sends (no coordinated omission)", func() {
		trace := evenTrace(6, 20) // arrivals at 0,20,40,60,80,100ms
		sender := slowSender{delay: 300 * time.Millisecond}

		t0 := time.Now()
		rows := Replay(context.Background(), sender, trace, ReplayOptions{Arm: "off"})
		Expect(rows).To(HaveLen(6))

		// Each request must have been dispatched near its scheduled offset, even though every response hangs for 300ms.
		//
		// A closed-loop client would serialize the 300ms hangs and push send N to roughly N*300ms; open-loop keeps it near N*20ms.
		for _, r := range rows {
			sentOffsetMs := (r.SendUnixNanos - t0.UnixNano()) / int64(time.Millisecond)
			Expect(sentOffsetMs).To(BeNumerically("~", r.ScheduledOffsetMs, 80),
				"request %d sent at %dms but scheduled for %dms", r.Index, sentOffsetMs, r.ScheduledOffsetMs)
		}
	})

	It("records first-token and end timestamps so TTFT comes from raw rows", func() {
		sender := stubSender{ttftDelay: 5 * time.Millisecond, totalDelay: 20 * time.Millisecond, outputTokens: 12, status: 200}
		rows := Replay(context.Background(), sender, evenTrace(3, 5), ReplayOptions{Arm: "kv-aware"})
		Expect(rows).To(HaveLen(3))
		for _, r := range rows {
			ttft, ok := r.TTFTNanos()
			Expect(ok).To(BeTrue())
			Expect(ttft).To(BeNumerically(">", 0))
			e2e, ok := r.E2ENanos()
			Expect(ok).To(BeTrue())
			Expect(e2e).To(BeNumerically(">=", ttft))
			Expect(r.OutputTokens).To(Equal(12))
			Expect(r.EstInputTokens).To(Equal(50)) // ceil(200/4)
		}
	})

	It("records a timed-out request as an error row rather than dropping it", func() {
		sender := stubSender{status: 0, errorKind: "timeout"}
		rows := Replay(context.Background(), sender, evenTrace(2, 5), ReplayOptions{Arm: "off"})
		Expect(rows).To(HaveLen(2))
		for _, r := range rows {
			Expect(r.ErrorKind).To(Equal("timeout"))
			_, ok := r.TTFTNanos()
			Expect(ok).To(BeFalse())
			Expect(r.SendUnixNanos).NotTo(BeZero()) // still recorded
		}
	})
})

// stubSender returns a fixed result after optional delays, on the same clock the scheduler used.
type stubSender struct {
	ttftDelay    time.Duration
	totalDelay   time.Duration
	outputTokens int
	status       int
	errorKind    string
}

func (s stubSender) Send(ctx context.Context, row TraceRow, sendUnixNanos int64) SendResult {
	if s.errorKind != "" {
		return SendResult{HTTPStatus: s.status, ErrorKind: s.errorKind}
	}
	first := sendUnixNanos + int64(s.ttftDelay)
	end := sendUnixNanos + int64(s.totalDelay)
	return SendResult{FirstTokenUnixNanos: first, EndUnixNanos: end, OutputTokens: s.outputTokens, HTTPStatus: s.status}
}

var _ = Describe("WriteRawRows and ReadRawRows", func() {
	It("round-trips raw evidence through JSON Lines", func() {
		rows := Replay(context.Background(), stubSender{ttftDelay: time.Millisecond, totalDelay: 2 * time.Millisecond, outputTokens: 4, status: 200}, evenTrace(4, 2), ReplayOptions{Arm: "static-cap"})
		var buf bytes.Buffer
		Expect(WriteRawRows(&buf, rows)).To(Succeed())
		got, err := ReadRawRows(bytes.NewReader(buf.Bytes()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(rows))
	})
})
