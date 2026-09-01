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
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// WorkloadImage is the image the trace jobs run, pinned by manifest-list digest.
//
// A mutable tag lets the image drift between runs, so the same study re-run months later could execute
// different bytes; pinning the multi-arch index digest freezes exactly what runs while still resolving on
// whatever architecture the node uses. WorkloadImage is exported so a caller that must check it against its
// own copy has no copy to keep.
//
// It moved from python:3.12-alpine to python:3.12-slim when the workload gained its device path, and the
// reason is musl. nvidia-container-toolkit does not ship a CUDA runtime into the container; it bind-mounts
// the HOST's driver libraries, which are linked against glibc. On an alpine base libcuda.so.1 is present and
// unloadable, so every run would have taken the CPU fallback with dev=no-libcuda and looked exactly like a
// node with no GPU. That is the worst possible failure here: silent, plausible, and indistinguishable from
// the honest answer.
const WorkloadImage = "python:3.12-slim@sha256:2c941e860699f878900b0edc2403613c234d4b32eda3cc9fa7036991a2a63c4a"

// workloadScript is the trace's workload. It tries to compute on the DEVICE, falls back to the CPU, and says
// which of the two it did.
//
// The self-report is the point of this file. Before it, the harness had no way to distinguish a run where the
// GPU worked from one where the same Python fell back to the CPU on a card the scheduler had reserved and
// nothing touched -- both produce a healthy iteration count and a plausible number, which is why a GPU run
// used to come back byte-identical to a kind run. The provenance a record carries is now this string's
// content rather than a literal in the writer, so the record cannot claim a kernel that never launched.
//
// The device path is the CUDA DRIVER API through ctypes, not torch or cupy, and that is a cost decision as
// much as a dependency one: those images are gigabytes, and the pull lands inside the window the experiment
// measures. libcuda.so.1 is injected by nvidia-container-toolkit for any container that requests a GPU, so
// the device path adds nothing to the image at all. The kernel is PTX loaded with cuModuleLoadData and
// JIT-compiled by the driver, which is the same mechanism Numba and PyTorch's JIT paths use.
//
// The PTX is compiled by hack/verify-ptx.sh, which stores an attestation keyed to a hash of the PTX it
// compiled; TestTheEmbeddedPTXWasCompiled refuses a mismatch. That
// check runs without a GPU and is the difference between a session that fails on a typo and one that does
// not. It used to be a claim in this comment with no check anywhere in the repository, which a review found:
// the one verification the file named as available without hardware was the one it did not have.
//
// The target is sm_75 -- Turing, which is the T4 in the instance type this study rents -- and not a lower
// floor. CUDA 13 dropped Maxwell and Pascal outright, so a driver from that generation refuses a
// .target sm_50 module even when the physical card is newer. A compatibility floor below the oldest card
// this lab could plausibly rent buys nothing and can only refuse.
//
// .version travels WITH the target and cannot be left behind: ISA 6.0 is CUDA 9.0 and predates Turing, so
// `.version 6.0 / .target sm_75` is rejected outright with "does not support". That pairing shipped for
// several minutes, and the ptxas test below is what found it -- which is the argument for the test rather
// than for the comment that used to stand in its place.
//
// The kernel arguments are BOUND INTO the launch closure, and that default argument is load-bearing rather
// than style. kernelParams is an array of raw addresses: ctypes copies the integer and keeps no reference,
// and the closure captures only the names it mentions. Without the binding, buf and inner are freed the
// moment cuda() returns, and every launch in the loop dereferences reclaimed memory -- while the FIRST
// launch, the one inside cuda() that sets dev="ok", succeeds because they are still alive. So the workload
// would report a healthy device path and then either abort with an illegal address or, worse, run with a
// garbage iteration count: one launch doing a hundred thousand times the intended work blocks
// cuCtxSynchronize past the grace period with the GIL released, which makes the HONOURING arm miss its
// SIGTERM and exit 137 like the ignoring one. That inverts the study's only experimental axis. Demonstrated
// with a control before it was fixed, and TestTheKernelArgumentsOutliveTheirBuilder is the regression.
//
// It stays runnable with NO device, and that is forced rather than chosen: the termination canary strips the
// GPU limit from its probe Pods so they can schedule on a node whose devices are spoken for, and a workload
// that could not start without CUDA would break the gate every run depends on. So a missing device is an
// ordinary fallback rather than an error -- but it is a REPORTED one, and dev= names which of the eight
// driver calls refused.
//
// The count and the report are written to /dev/termination-log from INSIDE the loop.
//
// The kubelet copies that file into the container's terminated status, which the collector already watches --
// soleTerminated reads the same struct. So the report arrives inside the Pod object rather than needing the
// logs subresource, a second client, extra RBAC, and a read that races teardown. Writing it from inside the
// loop rather than from the termination handler matters because the ignoring arm is SIGKILLed at the grace
// boundary and SIGKILL cannot be handled: a report written only on the way out would be missing from exactly
// the arm whose discarded work the experiment is about.
//
// Marking is time-based rather than every N iterations because the two paths differ in rate by orders of
// magnitude -- a kernel launch is milliseconds, a 50000-step Python float loop is not -- and a fixed
// iteration cadence would rewrite the file hundreds of times a second on the device path.
//
// The handler is installed BEFORE the device path runs. Context creation takes a noticeable fraction of a
// second on a cold card, and a SIGTERM arriving in that window against no handler kills the honoring arm
// under the ignoring arm's contract.
//
// Work per launch is FIXED -- 1024 blocks of 256 threads, 20000 fused multiply-adds each -- rather than
// calibrated to the card. An adaptive size would make "one iteration" mean something different on every run,
// and discarded iterations are compared across runs.
//
// It is carried in the command rather than baked into an image on purpose. The command is part of the Pod
// template the termination canary fingerprints, so changing the workload forces a re-take; an image would
// only do that when its digest moved, and a script edited inside the same tag would not move it.
const workloadScript = `import ctypes,signal,sys,time
seconds=float(sys.argv[1]); honor=sys.argv[2]=="honor"
n=0; kind="cpu-float"; dev="not-attempted"
PTX=b""".version 6.3
.target sm_75
.address_size 64
.visible .entry burn(.param .u64 p, .param .u32 iter)
{
.reg .pred %p<2>;
.reg .f32 %f<3>;
.reg .b32 %r<7>;
.reg .b64 %rd<5>;
ld.param.u64 %rd1, [p];
ld.param.u32 %r2, [iter];
cvta.to.global.u64 %rd2, %rd1;
mov.u32 %r3, %ctaid.x;
mov.u32 %r4, %ntid.x;
mov.u32 %r5, %tid.x;
mad.lo.s32 %r6, %r3, %r4, %r5;
mul.wide.s32 %rd3, %r6, 4;
add.s64 %rd4, %rd2, %rd3;
ld.global.f32 %f1, [%rd4];
mov.f32 %f2, 0f3F800001;
mov.u32 %r1, 0;
LOOP:
fma.rn.f32 %f1, %f1, %f2, %f2;
add.s32 %r1, %r1, 1;
setp.lt.s32 %p1, %r1, %r2;
@%p1 bra LOOP;
st.global.f32 [%rd4], %f1;
ret;
}
"""
try: tl=open("/dev/termination-log","w")
except Exception: tl=None
def msg(): return "iters=%d kind=%s dev=%s"%(n,kind,dev)
def mark():
    if tl is None: return
    tl.seek(0); tl.write(msg()); tl.truncate(); tl.flush()
def stop(*_):
    mark(); print("terminated "+msg(),flush=True); sys.exit(EXITCODE)
if honor: signal.signal(signal.SIGTERM,stop)
THREADS=1024*256
def cuda():
    global dev
    def bad(rc,tag):
        global dev
        if rc==0: return False
        dev=tag; return True
    try: lib=ctypes.CDLL("libcuda.so.1")
    except Exception: dev="no-libcuda"; return None
    lib.cuMemAlloc_v2.argtypes=[ctypes.POINTER(ctypes.c_ulonglong),ctypes.c_size_t]
    lib.cuMemsetD32_v2.argtypes=[ctypes.c_ulonglong,ctypes.c_uint,ctypes.c_size_t]
    lib.cuLaunchKernel.argtypes=[ctypes.c_void_p,ctypes.c_uint,ctypes.c_uint,ctypes.c_uint,
        ctypes.c_uint,ctypes.c_uint,ctypes.c_uint,ctypes.c_uint,ctypes.c_void_p,
        ctypes.POINTER(ctypes.c_void_p),ctypes.c_void_p]
    if bad(lib.cuInit(0),"cuinit-failed"): return None
    d=ctypes.c_int(0)
    if bad(lib.cuDeviceGet(ctypes.byref(d),0),"no-device"): return None
    ctx=ctypes.c_void_p()
    if bad(lib.cuCtxCreate_v2(ctypes.byref(ctx),0,d),"ctx-failed"): return None
    mod=ctypes.c_void_p()
    if bad(lib.cuModuleLoadData(ctypes.byref(mod),PTX),"ptx-load-failed"): return None
    fn=ctypes.c_void_p()
    if bad(lib.cuModuleGetFunction(ctypes.byref(fn),mod,b"burn"),"no-kernel"): return None
    buf=ctypes.c_ulonglong(0)
    if bad(lib.cuMemAlloc_v2(ctypes.byref(buf),ctypes.c_size_t(THREADS*4)),"alloc-failed"): return None
    if bad(lib.cuMemsetD32_v2(buf,ctypes.c_uint(0x3F800000),ctypes.c_size_t(THREADS)),"memset-failed"): return None
    inner=ctypes.c_uint(20000)
    args=(ctypes.c_void_p*2)(ctypes.cast(ctypes.byref(buf),ctypes.c_void_p),
                             ctypes.cast(ctypes.byref(inner),ctypes.c_void_p))
    def launch(_keep=(buf,inner,args,mod,ctx)):
        rc=lib.cuLaunchKernel(fn,1024,1,1,256,1,1,0,None,args,None)
        return rc if rc!=0 else lib.cuCtxSynchronize()
    if bad(launch(),"launch-failed"): return None
    dev="ok"; return launch
launch=cuda()
if launch is not None: kind="cuda-fma"
end=time.monotonic()+seconds; last=time.monotonic(); x=1.0
mark()
while time.monotonic()<end:
    if launch is None:
        for _ in range(50000): x=(x*1.0000001)%1000000.0
    else:
        rc=launch()
        if rc!=0:
            dev="launch-failed-midrun"; mark(); print("aborted "+msg(),flush=True); sys.exit(1)
    n+=1
    t=time.monotonic()
    if t-last>0.5: last=t; mark(); print(msg(),flush=True)
mark(); print("finished "+msg(),flush=True)`

// localQueueName is the deterministic LocalQueue name a tenant's jobs are admitted through.
//
// It must match the name BuildFixtures uses for that tenant's LocalQueue, so a submitted job lands in the
// queue whose policy variant is under test.
func localQueueName(tenant string) string {
	return "ql-" + tenant
}

// TerminationContract is how the rendered workload responds to SIGTERM.
//
// It is an explicit parameter because the lab's original sleeper could not be preempted at all: "sh -c
// sleep N" execs sleep as PID 1, and a container's PID 1 gets no default signal disposition, so SIGTERM is
// ignored and only the post-grace SIGKILL can stop it.
// A measured "preemption" against such a workload is not a preemption, so the contract has to be chosen
// deliberately rather than inherited from how the command happened to be written.
type TerminationContract string

const (
	// HonorsSIGTERM keeps the shell as PID 1 with a TERM trap, so a preemption actually stops the work.
	HonorsSIGTERM TerminationContract = "HonorsSIGTERM"
	// IgnoresSIGTERM is the original command, retained deliberately as the contrast arm.
	IgnoresSIGTERM TerminationContract = "IgnoresSIGTERM"
)

// termExitCode is the exit status the honoring workload uses when the trap fires.
//
// It is the conventional 128+SIGTERM, and it matters for measurement rather than convention: exiting zero
// would leave the Pod in phase Succeeded, indistinguishable from natural completion, which is exactly the
// ambiguity that let a completed run be reported as discarded work.
const termExitCode = 143

// RenderMLTrainingJob renders a trace row with the original ignoring contract.
//
// The default is deliberately the ignoring form so no existing caller silently switches arms; callers that
// want a preemptible workload must ask for it.
func RenderMLTrainingJob(row TrainingTraceRow, namespace string) (*platformv1.MLTrainingJob, error) {
	// The contract is a constant here, so the error cannot fire; it is returned rather than dropped so this
	// wrapper does not become the one place an unknown contract is silently tolerated.
	return RenderMLTrainingJobWithContract(row, namespace, IgnoresSIGTERM)
}

// RenderMLTrainingJobWithContract turns one immutable trace row into the MLTrainingJob to submit into
// namespace, with an explicit workload termination contract.
//
// The workload attempts the CUDA driver API and falls back to a CPU compute loop, in a digest-pinned Python
// image. This comment described it as CPU-only for several commits after it gained the device path, which is
// the same drift the pages were audited for. The change was deliberate: a sleeper holds the quota and computes nothing, so on real
// hardware "GPU-seconds discarded" would be GPU-seconds of discarded BOOKING, and nothing in the harness
// could tell the two apart. The loop also reports its progress, which is what turns a discarded GPU-second
// into a discarded unit of work.
//
// It must remain RUNNABLE WITHOUT A DEVICE, and that is forced rather than chosen: the termination canary
// strips the GPU limit from its probe Pods so they can schedule on a node whose devices are spoken for, and
// a workload requiring CUDA to start would break the gate every run depends on. That is why the device path
// is an attempt with a fallback rather than a requirement -- and why the fallback is REPORTED, so a run that
// took it cannot be mistaken for one that did not.
//
// The study still runs on kind with simulated nvidia.com/gpu capacity and no real GPU; what is under test is
// Kueue admission and reclaim, not GPU computation.
//
// Parallelism and completions are pinned to 1 so a row's gpuCount is exactly its demand (one Pod), which the
// occupancy and demand-satisfaction accounting assumes.
func RenderMLTrainingJobWithContract(
	row TrainingTraceRow, namespace string, contract TerminationContract,
) (*platformv1.MLTrainingJob, error) {
	command, err := sleeperCommand(row.DurationSec, contract)
	if err != nil {
		return nil, err
	}
	return &platformv1.MLTrainingJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      row.Name,
			Namespace: namespace,
			// The trace index and tenant are stamped as labels so the list/watch collector can join an
			// observed object back to its trace row without parsing the name.
			Labels: map[string]string{
				"queuelab.gpu-platform/trace-index": fmt.Sprintf("%d", row.Index),
				"queuelab.gpu-platform/tenant":      row.Tenant,
			},
		},
		Spec: platformv1.MLTrainingJobSpec{
			Queue:       localQueueName(row.Tenant),
			Image:       WorkloadImage,
			Command:     command,
			GPUCount:    int32(row.GPUCount),
			Parallelism: 1,
			Completions: 1,
		},
	}, nil
}

// sleeperCommand builds the container command for a duration under the given termination contract.
//
// The two arms differ by ONE argument, and that is deliberate. The ignoring arm installs no handler at all,
// because the reason the old `sh -c "sleep N"` ignored SIGTERM was never the command: PID 1 has its default
// signal dispositions dropped by the kernel, so a process that registers nothing is a process that ignores.
// Spelling it as an explicit SIG_IGN would ignore for a DIFFERENT reason than the shell form did, and the
// arms would then differ by two things rather than one.
//
// Measured on kind with a 15 s grace before this was written: no handler survived 16 s and was killed at the
// boundary, a handler exited in 2 s.
//
// The contract is the experimental axis of this study, so an unrecognized value must not fall through to the
// ignoring arm: that would run the contrast arm under the honoring arm's label and produce a plausible wrong
// result, which is the exact failure class the measurement work exists to eliminate.
func sleeperCommand(durationSec int, contract TerminationContract) ([]string, error) {
	// Substituted rather than formatted: the script is full of Python %d verbs and handing it to fmt.Sprintf
	// makes Go try to interpret them, which go vet catches and a reader would not.
	script := strings.Replace(workloadScript, "EXITCODE", strconv.Itoa(termExitCode), 1)
	switch contract {
	case HonorsSIGTERM:
		return []string{"python3", "-c", script, strconv.Itoa(durationSec), "honor"}, nil
	case IgnoresSIGTERM:
		return []string{"python3", "-c", script, strconv.Itoa(durationSec), "ignore"}, nil
	default:
		return nil, fmt.Errorf("unknown TerminationContract %q", contract)
	}
}
