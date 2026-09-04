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
	"flag"
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// power reports what the incremental-value gate can and cannot detect at a given repetition count.
//
// The question had never been asked: four repetitions were chosen and the design was written around them.
// A cold review pointed out that if four cannot detect the effect the experiment is looking for, every other
// preparation is beside the point and the run should not be bought.
//
// It simulates the gate the design actually pre-registered -- point estimate <= 0.90 AND bootstrap CI upper
// bound < 1.0 -- against the shipped BootstrapCI rather than a model of it, so the answer describes the code
// that will judge the run.
func power(args []string) error {
	fs := flag.NewFlagSet("power", flag.ExitOnError)
	reps := fs.Int("reps", 4, "repetitions per arm")
	trials := fs.Int("trials", 4000, "simulated experiments per cell")
	seed := fs.Uint64("seed", 42, "simulation seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reps < 1 {
		return fmt.Errorf("-reps must be at least 1")
	}

	r := rand.New(rand.NewPCG(*seed, 7))
	// Per-repetition ratios are modelled lognormal: a ratio of two positive latencies cannot go negative, and
	// the observed per-repetition p99s are tightly clustered rather than symmetric around zero.
	simulate := func(trueRatio, cv float64) float64 {
		sigma := math.Sqrt(math.Log(1 + cv*cv))
		mu := math.Log(trueRatio) - sigma*sigma/2
		passed := 0
		for range *trials {
			vals := make([]float64, *reps)
			mean := 0.0
			for i := range vals {
				vals[i] = math.Exp(mu + sigma*r.NormFloat64())
				mean += vals[i]
			}
			mean /= float64(*reps)
			ci := bench.BootstrapCI(vals, 2000, int64(r.Uint64()), 0.05)
			if mean <= 0.90 && ci.Valid && ci.Hi < 1.0 {
				passed++
			}
		}
		return float64(passed) / float64(*trials)
	}

	cvs := []float64{0.001, 0.05, 0.10, 0.20}
	fmt.Printf("Incremental-value gate at %d repetitions: point estimate <= 0.90 AND bootstrap CI hi < 1.0\n\n", *reps)

	fmt.Println("Power -- how often a real effect is detected")
	fmt.Printf("%-12s", "true C/B")
	for _, cv := range cvs {
		fmt.Printf("%12s", fmt.Sprintf("cv=%.3f", cv))
	}
	fmt.Println()
	for _, tr := range []float64{0.95, 0.90, 0.85, 0.80, 0.70} {
		fmt.Printf("%-12.2f", tr)
		for _, cv := range cvs {
			fmt.Printf("%11.1f%%", simulate(tr, cv)*100)
		}
		fmt.Println()
	}

	fmt.Println("\nFalse positive -- how often the gate fires with no effect at all (true C/B = 1.00)")
	fmt.Printf("%-12s", "")
	for _, cv := range cvs {
		fmt.Printf("%12s", fmt.Sprintf("cv=%.3f", cv))
	}
	fmt.Println()
	fmt.Printf("%-12s", "rate")
	for _, cv := range cvs {
		fmt.Printf("%11.1f%%", simulate(1.00, cv)*100)
	}
	fmt.Println()

	fmt.Printf(`
Two things this says, neither of which more repetitions would change.

At a true ratio of exactly 0.90 the gate fires about half the time, at every repetition count and every
variability. That is not a sample-size shortfall: the gate requires the point estimate to land at or below
the very value the effect is assumed to take, so it is a coin flip by construction. Buying repetitions
sharpens the estimate around 0.90 and leaves the coin exactly as fair. The design's number is therefore the
smallest effect it can be SURE of missing half the time -- an effect of 0.85 or better is detected almost
always at %d repetitions.

The percentile bootstrap over %d values is anti-conservative once the per-repetition ratios scatter: at a
coefficient of variation of 0.20 the gate fires on no effect at all more often than the nominal 5%%. The
2026-09-03 pilot measured 0.001 for the contended arms and 0.056 for the isolation-like ones, well inside
the safe region, and the report now refuses the incremental check if a run comes back scattered enough to
leave it.
`, *reps, *reps)
	return nil
}
