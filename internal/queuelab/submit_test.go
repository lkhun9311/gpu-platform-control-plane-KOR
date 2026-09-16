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

package queuelab

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRenderMLTrainingJobMatchesTraceAndQueue(t *testing.T) {
	rows := ReclaimScenario(true, 600)
	borrow := rows[1] // a2-borrow, tenant-a
	job, err := RenderMLTrainingJob(borrow, "run-ns")
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if job.Name != "a2-borrow" || job.Namespace != "run-ns" {
		t.Fatalf("job identity = %s/%s", job.Namespace, job.Name)
	}
	// The job must target the same LocalQueue name the fixtures created for its tenant, or it would never
	// be admitted through the policy variant under test.
	if job.Spec.Queue != "ql-tenant-a" {
		t.Fatalf("queue = %q, want ql-tenant-a", job.Spec.Queue)
	}
	if job.Spec.GPUCount != 1 {
		t.Fatalf("gpuCount = %d, want 1", job.Spec.GPUCount)
	}
	if job.Spec.Parallelism != 1 || job.Spec.Completions != 1 {
		t.Fatalf("parallelism/completions must be pinned to 1 so gpuCount is the demand")
	}
	// The duration is an ARGUMENT rather than text inside a shell string, which is what lets the two arms
	// share one script: a workload spelled per-arm could drift between them without the compiler noticing.
	// The duty cycle joins it for the same reason, and is present even at 1.0 so one experiment has one
	// spelling -- the command is what the termination canary fingerprints.
	if len(job.Spec.Command) != 6 || job.Spec.Command[0] != "python3" || job.Spec.Command[3] != "600" {
		t.Fatalf("workload command = %v, want python3 -c <script> 600 <contract> <duty>", job.Spec.Command)
	}
	if job.Spec.Command[5] != "1" {
		t.Fatalf("duty argument = %q, want \"1\" for a trace row that declares none", job.Spec.Command[5])
	}
	if job.Labels["queuelab.gpu-platform/trace-index"] != "1" {
		t.Fatalf("trace-index label = %q, want 1", job.Labels["queuelab.gpu-platform/trace-index"])
	}
}

func TestRenderMLTrainingJobQueuePerTenant(t *testing.T) {
	owner := ReclaimScenario(true, 600)[2] // b1-owner, tenant-b
	job, err := RenderMLTrainingJob(owner, "run-ns")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if job.Spec.Queue != "ql-tenant-b" {
		t.Fatalf("tenant-b job should target ql-tenant-b, got %q", job.Spec.Queue)
	}
	// The fixtures must create exactly this LocalQueue name for tenant-b, so submit and fixtures agree.
	fs, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r", Namespace: "run-ns"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lq := range fs.LocalQueue {
		if lq.Name == job.Spec.Queue {
			found = true
		}
	}
	if !found {
		t.Fatalf("submit targets queue %q but no fixture LocalQueue has that name", job.Spec.Queue)
	}
}

func TestRenderMLTrainingJobTerminationContract(t *testing.T) {
	row := TrainingTraceRow{Index: 0, Name: "a1", Tenant: "tenant-a", GPUCount: 1, DurationSec: 40}

	ignoring, err := RenderMLTrainingJobWithContract(row, "queuelab", IgnoresSIGTERM)
	if err != nil {
		t.Fatalf("render ignoring: %v", err)
	}
	honoring, err := RenderMLTrainingJobWithContract(row, "queuelab", HonorsSIGTERM)
	if err != nil {
		t.Fatalf("render honoring: %v", err)
	}

	// The arms must differ by EXACTLY ONE element, and that is the property the whole contrast rests on. A
	// second difference — a different script, a different duration, a different interpreter — would make the
	// measured gap attributable to something other than the termination contract, which is the one thing this
	// study varies.
	if len(ignoring.Spec.Command) != len(honoring.Spec.Command) {
		t.Fatalf("the arms have different command lengths: %d and %d",
			len(ignoring.Spec.Command), len(honoring.Spec.Command))
	}
	diff := 0
	for i := range ignoring.Spec.Command {
		if ignoring.Spec.Command[i] != honoring.Spec.Command[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("the arms differ in %d command elements, want exactly 1:\n ignore=%q\n honor=%q",
			diff, ignoring.Spec.Command, honoring.Spec.Command)
	}
	if ignoring.Spec.Command[4] != "ignore" || honoring.Spec.Command[4] != "honor" {
		t.Fatalf("the differing element must be the contract argument: %q vs %q",
			ignoring.Spec.Command[4], honoring.Spec.Command[4])
	}

	// The ignoring arm must install NOTHING. The reason the old `sh -c "sleep N"` ignored SIGTERM was never
	// the command: PID 1 has its default signal dispositions dropped by the kernel, so a process registering
	// no handler is a process that ignores. An explicit SIG_IGN would ignore for a different reason than the
	// form it replaces, and the two arms would then differ in mechanism as well as in label.
	script := honoring.Spec.Command[2]
	if strings.Contains(script, "SIG_IGN") {
		t.Fatalf("the workload installs SIG_IGN; the ignoring arm must ignore because PID 1 has no handler, " +
			"which is the mechanism the shell form used and the only one this translation preserves")
	}
	if !strings.Contains(script, "signal.signal(signal.SIGTERM") {
		t.Fatalf("the honouring arm installs no SIGTERM handler, so it would ignore the signal exactly as the "+
			"contrast arm does: %q", script)
	}
	// Progress is what turns occupancy from an assumption into evidence: a workload that computed nothing
	// reports iters=0 rather than being indistinguishable from one that saturated the device.
	//
	// The PERIODIC emit is asserted separately from the final ones, and the ignoring arm is why. It is killed
	// by SIGKILL at the end of the grace period, and SIGKILL cannot be handled — so no final line is ever
	// printed for it, and the last periodic line is the only progress evidence that arm leaves behind.
	// Checking for "iters=" alone passes on a script whose loop reports nothing, because the two exit lines
	// carry the same substring; that mutation survived until this assertion existed.
	//
	// The cadence is time-based rather than every N iterations because the two paths differ in rate by orders
	// of magnitude: a kernel launch is milliseconds, and a fixed iteration cadence would rewrite the
	// termination log hundreds of times a second on the device path.
	if !strings.Contains(script, "flush=True") {
		t.Fatal("progress is not flushed, so a killed workload's last line may never leave the buffer")
	}
	loopEmit := false
	for line := range strings.SplitSeq(script, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "if t-last>") && strings.Contains(t, "mark()") {
			loopEmit = true
		}
	}
	if !loopEmit {
		t.Fatal("the workload never reports progress from inside its loop; the ignoring arm is SIGKILLed and " +
			"prints no final line, so that arm would leave no evidence it computed anything at all")
	}
	got := strings.Join(honoring.Spec.Command, " ")
	// A trapped stop must be distinguishable from natural completion by exit status, or the ledger cannot
	// tell "the preemption stopped it" from "it finished on its own".
	if !strings.Contains(got, "sys.exit(143)") {
		t.Fatalf("honoring command %q must exit 143 on TERM so the stop is distinguishable", got)
	}

	// The default wrapper stays on the ignoring contract so existing callers do not silently change arms.
	defaulted, err := RenderMLTrainingJob(row, "queuelab")
	if err != nil {
		t.Fatalf("render default: %v", err)
	}
	if !slices.Equal(defaulted.Spec.Command, ignoring.Spec.Command) {
		t.Fatal("RenderMLTrainingJob must keep the IgnoresSIGTERM contract")
	}
}

// The exported renderer must refuse an unknown contract rather than take the process down with it.
//
// It is exported and takes the contract from its caller, so an invalid value is caller input, not an
// impossible internal state. It used to panic, which in a measurement binary loses the whole run over an
// argument the function could simply have rejected.
//
// Mutation that turns this red: restore the panic in sleeperCommand's default branch.
func TestRenderRefusesAnUnknownContract(t *testing.T) {
	row := TrainingTraceRow{Index: 0, Name: "x", Tenant: "t", GPUCount: 1, DurationSec: 10}

	job, err := RenderMLTrainingJobWithContract(row, "queuelab", TerminationContract("neither"))
	if err == nil {
		t.Fatal("an unknown contract was rendered instead of refused")
	}
	if job != nil {
		t.Fatal("a job was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Fatalf("the error does not name the contract it rejected: %v", err)
	}

	// The control: the two real contracts must still render, or the guard has become "refuse everything".
	for _, c := range []TerminationContract{HonorsSIGTERM, IgnoresSIGTERM} {
		if _, err := RenderMLTrainingJobWithContract(row, "queuelab", c); err != nil {
			t.Fatalf("contract %q was refused: %v", c, err)
		}
	}
}

// The shipped script is EXECUTED here, and nothing else in this repository can do what this test does.
//
// workloadScript is a Python program carried as a Go string. Every other check on it is textual: they assert
// that certain substrings are present, which a program with a syntax error on line 40 satisfies perfectly.
// Such a program fails at container start, every row of the trace fails to run, and the first place anyone
// finds out is the cluster -- on the GPU node, with the meter running.
//
// It asserts the REPORT rather than the count, because the report is the contract between this file and
// provenance.go. Those two agree by convention today: the workload writes "iters=N kind=K dev=D" and the
// parser expects it. A convention with no test is a convention that breaks on the next edit to either side,
// silently, into a run whose workload provenance is "unreported".
//
// The kind is not asserted to be the fallback. A developer machine with a GPU would take the device path and
// a test demanding cpu-float would fail there for being RIGHT -- so what is asserted is that the pair parses
// and the two sides agree, which holds on either kind of host.
func TestTheShippedWorkloadActuallyRunsAndReportsWhatTheParserExpects(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 on this host, so the shipped script cannot be executed here: %v", err)
	}
	job, err := RenderMLTrainingJobWithContract(TrainingTraceRow{
		Index: 0, Name: "probe", Tenant: "lab", GPUCount: 1, DurationSec: 2,
	}, "queuelab", IgnoresSIGTERM)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The command is run exactly as the Pod would run it, argv and all, rather than by re-deriving the script
	// here. A test that rebuilt the arguments would keep passing after the renderer stopped producing them.
	if job.Spec.Command[0] != "python3" {
		t.Fatalf("the rendered command does not start python3: %v", job.Spec.Command)
	}
	out, err := exec.Command(python, job.Spec.Command[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("the workload this build ships did not run:\n%v\n%s", err, out)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		t.Fatalf("the workload printed nothing:\n%s", out)
	}
	final := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(
		lastLine(string(out))), "finished "))
	iters, kind, device, _ := ReportFromMessage(final)
	if iters == nil {
		t.Fatalf("the parser could not read the report the shipped workload writes: %q\nfull output:\n%s",
			final, out)
	}
	if *iters <= 0 {
		t.Fatalf("the workload reported %d iterations after two seconds, so its loop did nothing", *iters)
	}
	switch kind {
	case KindCPUFloat:
		if device == DeviceOK {
			t.Fatalf("the fallback reported a successful device path: %q", final)
		}
	case KindCUDAFMA:
		if device != DeviceOK && device != "launch-failed-midrun" {
			t.Fatalf("the device kind reported a pre-launch failure: %q", final)
		}
	default:
		t.Fatalf("the workload reported a kind the parser accepted but this test does not know: %q", kind)
	}
	t.Logf("the shipped workload reported: %s", final)
}

// The honoring arm's exit status is what the ledger reads to tell a preemption from a natural completion, and
// it is produced by a signal handler in the shipped script rather than by anything in Go.
//
// Running it is the only way to know the handler is installed at all: the textual check beside this one sees
// "sys.exit(143)" in a script that never reaches signal.signal, and so does a script that installs the
// handler after a device-context creation that takes half a second on a cold card. That second case is not
// hypothetical -- the handler was moved above the device path for exactly it.
func TestTheHonoringArmActuallyExitsOnSIGTERM(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 on this host, so the shipped script cannot be executed here: %v", err)
	}
	job, err := RenderMLTrainingJobWithContract(TrainingTraceRow{
		Index: 0, Name: "probe", Tenant: "lab", GPUCount: 1, DurationSec: 120,
	}, "queuelab", HonorsSIGTERM)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cmd := exec.Command(python, job.Spec.Command[1:]...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Long enough that the loop is running and the handler is installed; the workload would otherwise still be
	// in its device probe and the signal would prove nothing about the handler.
	time.Sleep(2 * time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the honoring workload did not exit on SIGTERM after 120 s of work: %v\n%s", err, buf.String())
	}
	if exit.ExitCode() != termExitCode {
		t.Fatalf("the honoring workload exited %d, want %d: a stop that exits otherwise cannot be told from "+
			"natural completion\n%s", exit.ExitCode(), termExitCode, buf.String())
	}
	final := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lastLine(buf.String())), "terminated "))
	if iters, _, _, _ := ReportFromMessage(final); iters == nil {
		t.Fatalf("the preempted workload left no readable report, which is the evidence the arm exists to "+
			"produce: %q\n%s", final, buf.String())
	}
}

// lastLine is the final non-empty line of the workload's output.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// The embedded Python writes the device token and this package parses it back, so a rename on either side
// reclassifies a dying card as a run that never reached the device -- and nothing else would say so.
//
// goconst found the pair by counting the literal, which is the whole argument for the linter: three copies of
// a string that must agree is the same defect whether or not anyone has noticed it drift yet.
func TestTheWorkloadEmitsTheDeviceTokenThisPackageParses(t *testing.T) {
	if !strings.Contains(workloadScript, `dev="`+DeviceLaunchFailedMidrun+`"`) {
		t.Fatalf("the workload script never sets dev=%q, so a card that stops mid-run reports something this "+
			"package does not classify", DeviceLaunchFailedMidrun)
	}
	if !deviceStatuses[DeviceLaunchFailedMidrun] {
		t.Fatalf("%q is not in deviceStatuses, so the workload's own report reads as an unknown status",
			DeviceLaunchFailedMidrun)
	}
}

// TestAnUnsetDutyRendersWhatThisTraceAlwaysRendered keeps the new axis from moving the old experiment.
//
// The command is part of the Pod template the termination canary fingerprints, so a row that says nothing
// about duty must render one spelling, and it must be the spelling that means "compute throughout".
func TestAnUnsetDutyRendersWhatThisTraceAlwaysRendered(t *testing.T) {
	got, err := sleeperCommand(30, HonorsSIGTERM, DutyCycle(0).orFull())
	if err != nil {
		t.Fatalf("the historical row was refused: %v", err)
	}
	if n := len(got); n != 6 {
		t.Fatalf("command has %d parts, want 6 (python3 -c script seconds honor duty): %q", n, got)
	}
	if got[len(got)-2] != "honor" || got[len(got)-1] != "1" {
		t.Errorf("an unset duty rendered %q, want the full-duty spelling \"1\"", got[len(got)-2:])
	}
}

// TestADeclaredDutyReachesTheWorkload is the axis itself.
func TestADeclaredDutyReachesTheWorkload(t *testing.T) {
	got, err := sleeperCommand(30, IgnoresSIGTERM, DutyCycle(0.25))
	if err != nil {
		t.Fatalf("a quarter-duty row was refused: %v", err)
	}
	if got[len(got)-1] != "0.25" {
		t.Errorf("duty reached the workload as %q, want \"0.25\"", got[len(got)-1])
	}
	if !strings.Contains(got[2], "duty=float(sys.argv[3])") {
		t.Error("the rendered script does not read a duty argument at all")
	}
	if !strings.Contains(got[2], "time.sleep(min((1.0-duty)*PERIOD,rest))") {
		t.Error("the rendered script has no idle phase, so a declared duty would change nothing")
	}
}

// TestAnImpossibleDutyIsRefusedRatherThanClamped is the measurement rule applied to a knob.
//
// Clamping 1.5 to 1.0 would let a trace ask for something it did not get while the record reported the value
// it asked for. A refused trace is a trace nobody ran; a clamped one is a wrong number.
func TestAnImpossibleDutyIsRefusedRatherThanClamped(t *testing.T) {
	for _, d := range []DutyCycle{-1, 1.5, 2} {
		if _, err := sleeperCommand(30, HonorsSIGTERM, d); err == nil {
			t.Errorf("duty %v was accepted", float64(d))
		}
	}
	// Zero reaches sleeperCommand only if a caller skipped orFull, and it is refused there too: a row that
	// never computes cannot be told from one whose card was never observed.
	if _, err := sleeperCommand(30, HonorsSIGTERM, DutyCycle(0)); err == nil {
		t.Error("a zero duty was accepted; a row that never computes is indistinguishable from an unobserved one")
	}
}

// TestTheDeclaredDutyIsRecoverableFromTheWorkloadItself runs the embedded script and measures it.
//
// The knob is only worth having if it changes what the workload does, and the two tests above check the
// spelling rather than the behaviour: a script that read the argument and ignored it would pass both. This
// runs the real embedded source on the CPU fallback path -- no GPU, no container -- and checks that halving
// the duty roughly halves the work done in the same wall time.
//
// The tolerance is wide on purpose. A CPU iteration here is about a millisecond and the idle phase is a whole
// second, so the boundary between them quantises; what is being checked is that the duty is the thing
// deciding, not that it is exact.
func TestTheDeclaredDutyIsRecoverableFromTheWorkloadItself(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH; this test runs the workload rather than reading it")
	}
	script := strings.Replace(workloadScript, "EXITCODE", strconv.Itoa(termExitCode), 1)

	// Two whole periods, for the reason the device-path version of this records: a window that is not a
	// multiple of the workload's period truncates its last segment, and the ratio then measures the
	// truncation rather than the duty.
	seconds := strconv.FormatFloat(2*workloadPeriod(t), 'f', -1, 64)

	iters := func(duty string) int {
		t.Helper()
		out, err := exec.Command(python, "-c", script, seconds, "ignore", duty).CombinedOutput()
		if err != nil {
			t.Fatalf("duty %s: the workload did not finish: %v\n%s", duty, err, out)
		}
		m := regexp.MustCompile(`iters=(\d+)`).FindAllStringSubmatch(string(out), -1)
		if len(m) == 0 {
			t.Fatalf("duty %s: the workload reported no iteration count:\n%s", duty, out)
		}
		n, _ := strconv.Atoi(m[len(m)-1][1])
		return n
	}

	// Ordered rather than measured against a target ratio, for the reason the device-path version of this
	// records: iterations per second is not constant between a run that never rests and one that rests half
	// the time, so a ratio drifts for reasons the knob has nothing to do with. An ignored argument produces
	// three roughly equal counts, and that is what this has to exclude.
	full := iters("1")
	half := iters("0.5")
	quarter := iters("0.25")
	if full == 0 {
		t.Fatal("the workload did nothing at full duty, so nothing below means anything")
	}
	if half >= full*4/5 {
		t.Errorf("half duty did %d iterations against %d at full duty; the declared duty is not what decides "+
			"how much work happens", half, full)
	}
	if quarter >= half {
		t.Errorf("quarter duty did %d iterations and half duty %d; the declared duty does not order them",
			quarter, half)
	}
}

// TestAnArmsDutyReachesTheRenderedCommand is the step between a mapping and a manifest.
//
// DutyFor can be right while nothing applies it. The command is what the container runs and what the
// termination canary fingerprints, so this checks the value arrives there for each arm and each row.
func contractArg(c TerminationContract) string {
	if c == HonorsSIGTERM {
		return argHonor
	}
	return argIgnore
}

func TestAnArmsDutyReachesTheRenderedCommand(t *testing.T) {
	for _, tc := range []struct {
		arm  Arm
		row  string
		want string
	}{
		{ArmDFull, VictimRow, "1"},
		{ArmDQuarter, VictimRow, "0.25"},
		{ArmDQuarter, OwnRow, "1"},
		{ArmDQuarter, OwnerRow, "1"},
		{ArmAIgnore, VictimRow, "1"},
	} {
		duty, err := tc.arm.DutyFor(tc.row)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.arm, tc.row, err)
		}
		contract, err := tc.arm.ContractFor(tc.row)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.arm, tc.row, err)
		}
		// Rendered through RenderForArm, which is what the run path calls. Rendering with the pieces the
		// test resolved itself would check that this test can assemble an arm, not that the run path does.
		job, err := RenderForArm(tc.arm, TrainingTraceRow{
			Index: 1, Name: tc.row, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60,
		}, "ns")
		if err != nil {
			t.Fatalf("%s/%s render: %v", tc.arm, tc.row, err)
		}
		got := job.Spec.Command[len(job.Spec.Command)-1]
		if got != tc.want {
			t.Errorf("%s/%s renders duty %q, want %q", tc.arm, tc.row, got, tc.want)
		}
		// The other half of what the arm decides, checked in the same render so the two cannot drift apart.
		if arm := job.Spec.Command[len(job.Spec.Command)-2]; arm != contractArg(contract) {
			t.Errorf("%s/%s renders contract arm %q, want the spelling of %q", tc.arm, tc.row, arm, contract)
		}
		_ = duty
	}
}

// TestTheWorkloadDoesNotShareItsPeriodWithTheSampler holds the two files together.
//
// The workload's duty cycle has a period, and the exporter collects on one too. When they were equal a
// quarter-duty victim executing 22,093 real kernels was reported as working in zero of 104 samples: a
// sample landing in the idle part of the cycle landed there every time, because two processes at the same
// frequency do not drift through each other's phase.
//
// A comment in each file cannot hold that, because the two values live in different files and neither edit
// looks wrong beside the other. This reads the exporter's manifest and refuses a workload period that
// divides into it or is divided by it.
func TestTheWorkloadDoesNotShareItsPeriodWithTheSampler(t *testing.T) {
	m := regexp.MustCompile(`(?m)^PERIOD=([0-9.]+)$`).FindStringSubmatch(workloadScript)
	if m == nil {
		t.Fatal("the workload declares no PERIOD, so nothing here can check it against the sampler's")
	}
	period, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("PERIOD=%q is not a number", m[1])
	}

	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "dcgm-exporter", "daemonset.yaml"))
	if err != nil {
		t.Fatalf("cannot read the exporter manifest, so this check verifies nothing: %v", err)
	}
	cm := regexp.MustCompile(`(?m)^\s*-\s*"-c"\s*\n\s*-\s*"(\d+)"`).FindSubmatch(manifest)
	if cm == nil {
		t.Fatal("the exporter manifest carries no -c interval; if the flag was renamed this check is stale " +
			"and must be updated rather than deleted")
	}
	ms, err := strconv.Atoi(string(cm[1]))
	if err != nil {
		t.Fatalf("the exporter's -c value %q is not a number", cm[1])
	}
	sampler := float64(ms) / 1000

	// Equal is the case that was measured. Integer multiples in either direction are the same trap: the
	// phase still repeats, it just takes longer to come round.
	ratio := period / sampler
	if math.Abs(ratio-math.Round(ratio)) < 0.05 || math.Abs(1/ratio-math.Round(1/ratio)) < 0.05 {
		t.Errorf("the workload's period is %gs and the exporter collects every %gs, a ratio of %.3f; the "+
			"burst does not walk through the sampler's phase and a duty cycle can hide behind it entirely, "+
			"which is what session qlgpu-20260906-103327 measured", period, sampler, ratio)
	}
}
