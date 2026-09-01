// A CPU-only stand-in for libcuda.so.1 that VALIDATES what the workload passes it.
//
// It exists because ptxas checks PTX SYNTAX and nothing checks the ctypes boundary -- the argument types,
// the kernelParams array, the launch geometry, the allocation size, the memset pattern, the kernel name.
// That boundary is what actually runs on the rented card, and until this file no test in this repository
// had ever executed the workload's device path at all: the only end-to-end test runs on a host with no
// libcuda.so.1, takes the fallback branch, and never constructs a kernel argument.
//
// Every check here EXITS with a distinct status rather than returning an error code, because a CUDA error
// code would be reported by the workload as an ordinary device failure and the test would read it as a
// correct refusal. A shim that could be mistaken for a broken GPU would verify nothing.
//
// SHIM_FAIL_AT injects a failure at one named symbol so the workload's closed set of device-status tokens
// can be driven from the outside; SHIM_FAIL_LAUNCH_AFTER makes a later launch fail, which is the only way
// to reach launch-failed-midrun.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
static uint64_t g_buf_backing[1024*256];
static int g_launches = 0;
static const char *fail_at(void){ return getenv("SHIM_FAIL_AT"); }
static int should_fail(const char *sym){ const char *f=fail_at(); return f && !strcmp(f,sym); }
int cuInit(unsigned f){ (void)f; return should_fail("cuInit") ? 3 : 0; }
int cuDeviceGet(int *d,int o){ (void)o; *d=0; return should_fail("cuDeviceGet") ? 101 : 0; }
int cuCtxCreate_v2(void **c,unsigned f,int d){ (void)f;(void)d; *c=(void*)0x1; return should_fail("cuCtxCreate_v2")?1:0; }
int cuModuleLoadData(void **m,const void *img){
  if(should_fail("cuModuleLoadData")) return 218;
  if(!img || !strstr((const char*)img,".entry burn")){ fprintf(stderr,"SHIM: image is not the burn kernel\n"); exit(90); }
  if(!strstr((const char*)img,".target sm_75")){ fprintf(stderr,"SHIM: unexpected target\n"); exit(91); }
  *m=(void*)0x2; return 0; }
int cuModuleGetFunction(void **fn,void *m,const char *n){ (void)m;
  if(should_fail("cuModuleGetFunction")) return 500;
  if(strcmp(n,"burn")){ fprintf(stderr,"SHIM: wrong kernel name %s\n",n); exit(92); }
  *fn=(void*)0x3; return 0; }
int cuMemAlloc_v2(unsigned long long *p,size_t n){
  if(should_fail("cuMemAlloc_v2")) return 2;
  if(n!=1024UL*256UL*4UL){ fprintf(stderr,"SHIM: alloc %zu bytes, want %lu\n",n,1024UL*256UL*4UL); exit(93); }
  *p=(unsigned long long)(uintptr_t)g_buf_backing; return 0; }
int cuMemsetD32_v2(unsigned long long p,unsigned v,size_t n){ (void)p;
  if(should_fail("cuMemsetD32_v2")) return 1;
  if(v!=0x3F800000u){ fprintf(stderr,"SHIM: memset pattern 0x%X\n",v); exit(94); }
  if(n!=1024UL*256UL){ fprintf(stderr,"SHIM: memset count %zu\n",n); exit(95); }
  return 0; }
int cuLaunchKernel(void *f,unsigned gx,unsigned gy,unsigned gz,unsigned bx,unsigned by,unsigned bz,
                   unsigned sh,void *st,void **params,void **extra){
  (void)f;(void)st;(void)extra;
  if(should_fail("cuLaunchKernel")) return 700;
  if(gx!=1024||gy!=1||gz!=1||bx!=256||by!=1||bz!=1||sh!=0){
    fprintf(stderr,"SHIM: launch shape %u,%u,%u / %u,%u,%u shared=%u\n",gx,gy,gz,bx,by,bz,sh); exit(96); }
  if(!params||!params[0]||!params[1]){ fprintf(stderr,"SHIM: null kernelParams\n"); exit(97); }
  // THE CHECK THIS SHIM EXISTS FOR: kernelParams holds ADDRESSES of the two arguments. If the Python
  // objects behind them were freed, these dereference reclaimed memory.
  unsigned long long devptr = *(unsigned long long*)params[0];
  unsigned iters = *(unsigned*)params[1];
  if(devptr != (unsigned long long)(uintptr_t)g_buf_backing){
    fprintf(stderr,"SHIM: kernelParams[0] dereferences to 0x%llX, not the pointer cuMemAlloc returned "
                   "(0x%llX) -- the argument object was freed\n", devptr,
                   (unsigned long long)(uintptr_t)g_buf_backing); exit(20); }
  if(iters != 20000u){
    fprintf(stderr,"SHIM: kernelParams[1] dereferences to %u, not 20000 -- the argument object was freed\n",
            iters); exit(21); }
  g_launches++;
  const char *n=getenv("SHIM_FAIL_LAUNCH_AFTER");
  if(n && g_launches > atoi(n)) return 700;
  return 0; }
int cuCtxSynchronize(void){ return should_fail("cuCtxSynchronize") ? 700 : 0; }
