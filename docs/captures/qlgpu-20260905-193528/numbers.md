# qlgpu-20260905-193528

captured 2026-09-05T19:36:41Z from hack/qlgpu-20260905-193528/ (stamped from the evidence, not the clock)
instance: i-0017dd991dd262988

## cards

index, name, memory.total [MiB]
0, NVIDIA A10G, 23028 MiB
1, NVIDIA A10G, 23028 MiB
2, NVIDIA A10G, 23028 MiB
3, NVIDIA A10G, 23028 MiB

## why a preflight refused

+ echo '=== node container: does it see any card at all? ==='
=== node container: does it see any card at all? ===
+ docker exec qlgpu-worker nvidia-smi -L
GPU 0: NVIDIA A10G (UUID: GPU-ec973bb4-b7db-68e7-4fa2-1d5fa75eeac9)
GPU 1: NVIDIA A10G (UUID: GPU-ad84cb44-9978-efb6-9569-3ae5b912581c)
GPU 2: NVIDIA A10G (UUID: GPU-edcd8447-33fa-b5ef-cf71-05060a229f37)
GPU 3: NVIDIA A10G (UUID: GPU-cf8a578b-6649-ba98-ffd0-851f4ef18b57)
+ echo

+ echo '=== the mount that should have put them there ==='
=== the mount that should have put them there ===
+ docker exec qlgpu-worker ls -la /var/run/nvidia-container-devices/
total 0
drwxr-xr-x  2 root root   60 Sep  5 19:37 .
drwxr-xr-x 15 root root  320 Sep  5 19:37 ..
crw-rw-rw-  1 root root 1, 3 Sep  5 19:36 all
+ echo

+ echo '=== docker default runtime ==='
=== docker default runtime ===
+ docker info
+ grep -iA2 runtime
 Runtimes: runc io.containerd.runc.v2 nvidia
 Default Runtime: nvidia
 Init Binary: docker-init
 containerd version: db8809540e1a7a9da5d518876894933ff55692ab
+ echo

+ echo '=== pods ==='
=== pods ===
+ kubectl -n gpu-platform-control-plane-system get pods -o wide
NAME                                                             READY   STATUS             RESTARTS       AGE     IP           NODE           NOMINATED NODE   READINESS GATES
dcgm-exporter-mjg49                                              0/1     Running            0              5m5s    10.244.1.6   qlgpu-worker   <none>           <none>
gpu-platform-control-plane-controller-manager-64c56cfdc5-5kswx   1/1     Running            0              5m16s   10.244.1.4   qlgpu-worker   <none>           <none>
nvidia-device-plugin-jl5wj                                       0/1     CrashLoopBackOff   5 (114s ago)   5m5s    10.244.1.5   qlgpu-worker   <none>           <none>
+ echo

+ echo '=== device plugin ==='
=== device plugin ===
+ kubectl -n gpu-platform-control-plane-system describe ds nvidia-device-plugin

