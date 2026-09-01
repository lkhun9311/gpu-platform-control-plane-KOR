package queuelab

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// buildFakeCUDA compiles testdata/fake_libcuda.c into a libcuda.so.1 the workload will load.
//
// It skips rather than fails when there is no compiler, because the value here is the check itself and a
// build environment without gcc is not evidence about the workload. It does NOT skip when the compile
// fails: that is the shim being broken, and a silently skipped shim is exactly the reassurance this file
// exists to refuse.
func buildFakeCUDA(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		if cc, err = exec.LookPath("cc"); err != nil {
			t.Skip("no C compiler on this host, so the fake libcuda cannot be built here")
		}
	}
	dir := t.TempDir()
	so := filepath.Join(dir, "libcuda.so.1")
	out, err := exec.Command(cc, "-shared", "-fPIC", "-o", so, "testdata/fake_libcuda.c").CombinedOutput()
	if err != nil {
		t.Fatalf("the fake libcuda did not compile, so nothing below verifies anything: %v\n%s", err, out)
	}
	return dir
}

// runWorkload executes the SHIPPED command with the fake driver on the library path.
func runWorkload(t *testing.T, libDir string, env []string, seconds, arm string) (string, int) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on this host")
	}
	job, err := RenderMLTrainingJobWithContract(TrainingTraceRow{
		Index: 0, Name: "probe", Tenant: "lab", GPUCount: 1, DurationSec: 1,
	}, "queuelab", IgnoresSIGTERM)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	args := append([]string{}, job.Spec.Command[1:]...)
	args[len(args)-2], args[len(args)-1] = seconds, arm
	cmd := exec.Command(python, args...)
	cmd.Env = append(append(os.Environ(), "LD_LIBRARY_PATH="+libDir), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if err != nil {
		if !stderrAs(err, &exit) {
			t.Fatalf("the workload did not run: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}
	return strings.TrimSpace(string(out)), code
}

func stderrAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// The workload's DEVICE path, executed. Nothing in this repository had ever run it.
//
// TestTheShippedWorkloadActuallyRunsAndReportsWhatTheParserExpects runs on a host with no libcuda.so.1, so
// it takes the fallback branch at the first line of cuda() and never constructs a kernel argument, never
// loads the PTX, never launches anything. Its own comment claims nothing else can do what it does; that was
// true only of the fallback.
//
// The shim asserts, from the C side and with distinct exit statuses so a failure cannot be mistaken for a
// broken GPU: the module image is this file's PTX and targets sm_75, the kernel is named burn, the
// allocation is exactly THREADS*4 bytes, the memset pattern is 1.0f, the launch geometry is 1024x256 with
// no shared memory, and kernelParams[0] and [1] dereference to the pointer cuMemAlloc returned and to
// 20000.
//
// That last pair is the one worth having. kernelParams is an array of raw addresses; ctypes copies the
// integer and keeps no reference, so a build whose argument objects go out of scope hands the driver two
// dangling pointers. This test dereferences them on every launch.
func TestTheWorkloadsDevicePathRunsAgainstAFakeDriver(t *testing.T) {
	lib := buildFakeCUDA(t)
	out, code := runWorkload(t, lib, nil, "1", "ignore")
	if code != 0 {
		t.Fatalf("the device path exited %d:\n%s", code, out)
	}
	final := lastLine(out)
	iters, kind, device := ReportFromMessage(strings.TrimSpace(strings.TrimPrefix(final, "finished ")))
	if iters == nil {
		t.Fatalf("the device path left no readable report: %q\n%s", final, out)
	}
	if kind != KindCUDAFMA || device != DeviceOK {
		t.Fatalf("the workload reported kind=%q dev=%q against a driver that accepted everything", kind, device)
	}
	if *iters <= 0 {
		t.Fatalf("the device path reported %d launches", *iters)
	}
}

// Each way the driver can refuse produces its own token, and the tokens are what send an operator to the
// right place. They were a closed set nobody had ever driven from the driver's side.
func TestEachDriverRefusalProducesItsOwnToken(t *testing.T) {
	lib := buildFakeCUDA(t)
	for _, tc := range []struct{ symbol, token string }{
		{"cuInit", "cuinit-failed"},
		{"cuDeviceGet", "no-device"},
		{"cuCtxCreate_v2", "ctx-failed"},
		{"cuModuleLoadData", "ptx-load-failed"},
		{"cuModuleGetFunction", "no-kernel"},
		{"cuMemAlloc_v2", "alloc-failed"},
		{"cuMemsetD32_v2", "memset-failed"},
		{"cuLaunchKernel", "launch-failed"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			out, _ := runWorkload(t, lib, []string{"SHIM_FAIL_AT=" + tc.symbol}, "1", "ignore")
			final := strings.TrimSpace(strings.TrimPrefix(lastLine(out), "finished "))
			_, kind, device := ReportFromMessage(final)
			if device != tc.token {
				t.Fatalf("a driver refusing %s reported dev=%q, want %q\n%s", tc.symbol, device, tc.token, out)
			}
			// Every pre-launch refusal returns before the kind is set, so the workload must report the
			// fallback -- and the parser's cross-token rule refuses any other pairing outright.
			if kind != KindCPUFloat {
				t.Fatalf("a refusal at %s left kind=%q, which the parser pairs with no pre-launch failure",
					tc.symbol, kind)
			}
		})
	}

	// A card that works and then stops is the one failure that keeps the device kind, and it is the only
	// path to a non-zero exit from the loop rather than from the handler.
	out, code := runWorkload(t, lib, []string{"SHIM_FAIL_LAUNCH_AFTER=3"}, "5", "ignore")
	final := strings.TrimSpace(strings.TrimPrefix(lastLine(out), "aborted "))
	_, kind, device := ReportFromMessage(final)
	if kind != KindCUDAFMA || device != "launch-failed-midrun" {
		t.Fatalf("a mid-run launch failure reported kind=%q dev=%q\n%s", kind, device, out)
	}
	if code == 0 {
		t.Fatal("a workload whose card stopped mid-run exited zero, so a run would count it as a completion")
	}
}

// The kernel arguments must outlive the function that built them.
//
// This is a STRUCTURAL check, and the reason is worth stating: the defect is undefined behaviour that does
// not reproduce. `args._objects` is nil and the closure's free variables are `('args',)`, so the two
// argument objects are freed when cuda() returns and every launch dereferences reclaimed memory -- but the
// loop allocates nothing that lands on those blocks, and 2.9 million launches under two different Python
// allocators read the correct values. Forcing the reuse by hand corrupts them immediately.
//
// So the shim above cannot catch it, and a test that waited for it to bite would pass on a broken build.
// What is checked instead is the property that makes it safe: the launch closure keeps a reference. That is
// a weaker kind of test and it is the honest one for a bug whose manifestation is a matter of allocator
// luck on a machine costing money by the hour.
func TestTheKernelArgumentsOutliveTheirBuilder(t *testing.T) {
	script := workloadScript
	if !strings.Contains(script, "args=(ctypes.c_void_p*2)(") {
		t.Fatal("the workload no longer builds a kernelParams array; this test is checking a shape that " +
			"has moved, and must be rewritten rather than deleted")
	}
	i := strings.Index(script, "def launch(")
	if i < 0 {
		t.Fatal("the workload has no launch closure")
	}
	signature := script[i : strings.Index(script[i:], "\n")+i]
	for _, held := range []string{"buf", "inner", "args"} {
		if !strings.Contains(signature, held) {
			t.Fatalf("the launch closure does not hold %q: %s\n\nkernelParams stores raw addresses and keeps "+
				"no reference, so an argument object the closure does not name is freed when cuda() returns "+
				"and the driver is handed a dangling pointer", held, signature)
		}
	}
}

// The PTX in the tree must be the PTX somebody compiled.
//
// This used to shell out to ptxas and SKIP when there was none, which is how it behaves on every machine
// without a CUDA toolkit -- including this one. A check that disappears silently proves nothing, and a green
// suite was reading as evidence that the one verification submit.go names as available without a GPU had
// been done. Before that, the comment claimed the compilation had happened and no check existed anywhere in
// the repository at all.
//
// So the compilation is done deliberately by hack/verify-ptx.sh, which stores its result keyed to a hash of
// the PTX it compiled. This is the termination canary's shape: a stored qualification, refused the moment
// the thing it qualified changes. What it establishes is that this exact text compiled somewhere, for the
// listed targets, and that nobody has edited it since -- weaker than "ptxas ran just now", stronger than a
// skip, and honest about which.
//
// It earned its place immediately on the first attempt: moving the target to sm_75, which is the T4 this
// study rents, made .version 6.0 illegal because ISA 6.0 predates Turing. The pairing shipped for minutes.
//
// Mutation that turns this red: edit one character of the PTX without re-running the script.
func TestTheEmbeddedPTXWasCompiled(t *testing.T) {
	script := workloadScript
	_, after, ok := strings.Cut(script, `PTX=b"""`)
	if !ok {
		t.Fatal("the workload carries no PTX; this test is checking a shape that has moved")
	}
	body := after
	body = body[:strings.Index(body, `"""`)]
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))

	raw, err := os.ReadFile(filepath.Join("testdata", "ptx-verification.json"))
	if err != nil {
		t.Fatalf("no PTX verification on record: %v\nRun hack/verify-ptx.sh (it needs no GPU)", err)
	}
	var att struct {
		PTXSHA256    string   `json:"ptxSHA256"`
		PTXASVersion string   `json:"ptxasVersion"`
		Targets      []string `json:"targets"`
	}
	if err := json.Unmarshal(raw, &att); err != nil {
		t.Fatalf("the PTX verification does not parse: %v", err)
	}
	if att.PTXSHA256 != sum {
		t.Fatalf("the PTX in the tree is not the PTX that was compiled.\n  tree: %s\n  attested: %s\n"+
			"Run hack/verify-ptx.sh; it needs no GPU, and until it is run nothing establishes that this "+
			"kernel assembles for any architecture.", sum, att.PTXSHA256)
	}
	// The target the study actually rents. A verification for architectures the session will never see is
	// not a verification of the session.
	want := "sm_75"
	if !slices.Contains(att.Targets, want) {
		t.Fatalf("the PTX was verified for %v and not for %s, which is the T4 this study rents",
			att.Targets, want)
	}
	if strings.TrimSpace(att.PTXASVersion) == "" {
		t.Fatal("the verification names no ptxas, so nobody can say which assembler accepted this kernel")
	}
	t.Logf("PTX %s... compiled for %v by %s", sum[:16], att.Targets, att.PTXASVersion)
}
