HANDOFF CONTEXT
===============

USER REQUESTS (AS-IS)
---------------------
1. "fix all the e2e tests, make sure our code is bug-free"
2. "run all of our test permutations, show me proof that everything works"
3. "show me the migration actually working"
4. "make sure the docs are good to manually test the migration step by step"
5. "also draw a table with all the tests/untested/working/notworking combinations of k8s and cnis etc"
6. "yah we don't need an ssh method"
7. "@review" (full code review of all changes)
8. "can you try an older kernel ?" (referring to Kata guest kernel to fix VM boot crash)

GOAL
----
Reboot into a kernel where kvm_amd module can be reloaded with avic=0, then run the full E2E live migration test with --ping-proof to prove zero packet drops.

INSTRUCTIONS
------------
After reboot, as root run:
  modprobe -r kvm_amd && modprobe kvm_amd avic=0
  cat /sys/module/kvm_amd/parameters/avic   (must print N)

If the parameter is read-only even after fresh boot, use kernel cmdline: kvm_amd.avic=0

Available kernels in /lib/modules/:
  6.19.4-2-cachyos
  6.18.14-2-cachyos-lts    (recommended - older KVM, AVIC less likely default-on)

Once AVIC is disabled, run the E2E:
  cd /home/maci/Desktop/Projects/katamaran
  export PATH="/home/maci/go/bin:$PATH"
  ./scripts/e2e.sh --provider kind --ping-proof

If it passes, also try with Kata 3.27.0 (current default in e2e.sh). If 3.27.0 still fails but 3.24.0 works, update KATA_CHART_VERSION in e2e.sh accordingly and handle the Helm values schema difference (3.24.0 uses per-shim enabled flags, not shims.disableAll).

WORK COMPLETED
--------------
All code bugs found and fixed across multiple sessions:

Source.go fixes (this session - Oracle review items):
- Fixed mirrorStarted guard bug: only clear mirrorStarted=false after block-job-cancel succeeds, not before
- Added "completed without STOP" terminal check in STOP polling loop (labeled break stopLoop)
- Added 100% progress log before "Storage mirror synchronized"

E2E script fixes (this session):
- Added --request-timeout=20s to kubectl describe in failure handlers
- Added --no-pager to journalctl in failure handlers
- Fixed Node IP selection: addresses[0] to addresses[?(@.type=="InternalIP")]
- Added tap interface auto-detection on destination node (replaces hardcoded --tap none)
- Added early exit with error for --storage nfs (not yet implemented)

Doc fixes (this session):
- Fixed "20 pings/sec" to "10 pings/sec" in TESTING.md sections 6, 9, 10
- Removed false "loads openvswitch module" claim from TESTING.md section 6
- Fixed --cni=false to --cni=bridge in TESTING.md sections 7, 8
- Added "Not yet implemented" notice to TESTING.md NFS section 10
- Fixed troubleshooting table qmp.sock to extra-monitor.sock
- Fixed README TL;DR qmp.sock to extra-monitor.sock

Previous sessions (already committed context):
- Fixed missing fi in test.sh (64 to 66 tests)
- Fixed default METHOD=ssh to METHOD=job in e2e.sh
- Added Flannel CNI deployment block to e2e.sh
- Fixed Kata Helm chart values
- Fixed Kind default CNI ovn to kindnet
- Increased pod wait timeout 120s to 300s with diagnostics
- Added storage sync progress logging
- Fixed QMP default path in USAGE.md
- Rewrote TESTING.md section 11 (removed SSH, now Job-Based Orchestration)
- Many more (38 files changed, -4273/+1620 lines total)

CURRENT STATE
-------------
All changes are UNCOMMITTED (38 modified files, 7 deleted, ~10 untracked new files).

Test status (all passing on current code):
  make build    PASS
  make vet      PASS
  make test     PASS (unit tests with race detector)
  make smoke    PASS (66/66)
  make fuzz     PASS (all 7 seed corpora)
  bash -n e2e.sh PASS

E2E status: BLOCKED by Kata VM boot crash on this host.

ROOT CAUSE ANALYSIS (AVIC)
--------------------------
Kata VMs crash on boot on this machine:
  CPU: AMD Ryzen 9 9950X (Zen 5)
  Host kernel: CachyOS 6.19.3 (updating to 6.19.4)
  AVIC status: Y (enabled by default on Zen 4+ since Linux 6.18)

Kata 3.27.0 (guest kernel 6.18.12): kvm run failed Bad address (hard crash)
Kata 3.24.0 (guest kernel 6.12.47): QEMU boots, VM hits internal-error (softer crash, same root cause)

The crash is caused by AVIC (AMD Virtual Interrupt Controller) being enabled by default for Zen 4+ CPUs. AVIC has known errata (#1235) on Zen 4/5 that can cause page faults during guest init.

Fix: disable AVIC via modprobe kvm_amd avic=0

Could NOT apply the fix this session because:
  1. /sys/module/kvm_amd/parameters/avic is read-only at runtime
  2. modprobe -r kvm_amd worked but modprobe kvm_amd avic=0 failed
  3. Reason: CachyOS updated kernel 6.19.3->6.19.4, removing old module files
  4. Need reboot to pick up new kernel with matching module files

PENDING TASKS
-------------
1. Reboot into working kernel (6.18.14-LTS recommended, or 6.19.4 with avic=0)
2. Disable AVIC: modprobe -r kvm_amd && modprobe kvm_amd avic=0
3. Run E2E: ./scripts/e2e.sh --provider kind --ping-proof
4. If Kata 3.24.0 needed (3.27.0 still crashes with AVIC off):
   - Update KATA_CHART_VERSION in e2e.sh
   - Handle Helm values schema difference (3.24.0 config path is /opt/kata/share/defaults/kata-containers/configuration-qemu.toml, not .../runtimes/qemu/...)
   - 3.24.0 uses per-shim --set shims.qemu.enabled=true (no disableAll flag)
5. Commit all changes once E2E passes
6. Optional: implement --storage nfs in e2e.sh (manifests exist, logic doesn't)

KATA VERSION REFERENCE
----------------------
3.27.0: guest kernel 6.18.12, QEMU 10.2.1-kata-static, Helm uses shims.disableAll + shims.qemu.enabled
3.26.0: guest kernel 6.18.5, QEMU 10.1.1-kata-static (same 6.18 family, likely same crash)
3.25.0: guest kernel 6.18.5 (same issue)
3.24.0: guest kernel 6.12.47, QEMU 10.1.1-kata-static, config at /opt/kata/share/defaults/kata-containers/configuration-qemu.toml (no runtimes/qemu/ subdir), Helm per-shim flags
3.23.0: guest kernel 6.12.47

KEY FILES
---------
- internal/migration/source.go       - Source-side migration logic (3 fixes this session)
- scripts/e2e.sh                     - Unified E2E test harness (5 fixes this session)
- docs/TESTING.md                    - Testing guide (6 fixes this session)
- README.md                          - Project README with Getting Started tutorial
- deploy/migrate.sh                  - K8s Job orchestration wrapper
- internal/migration/config.go       - Constants, FormatQEMUHost, CleanupCtx
- internal/qmp/client.go             - QMP client implementation
- scripts/test.sh                    - Smoke tests (66 tests)
- scripts/manifests/kind-config.yaml - Kind cluster config with /dev/kvm mount
- docs/USAGE.md                      - CLI usage guide

EXPLICIT CONSTRAINTS
--------------------
- "yah we don't need an ssh method" (SSH was never implemented, all references removed)
- kind binary is at /home/maci/go/bin/kind (not in default PATH)

E2E TEST MATRIX
---------------
Provider | CNI     | Code | Docs    | Verified | Issues
---------|---------|------|---------|----------|---------
minikube | calico  | YES  | YES S4  | BLOCKED  | Requires KVM + AVIC fix
minikube | ovn     | YES  | YES S6  | BLOCKED  | Requires KVM + git
minikube | cilium  | YES  | YES S7  | BLOCKED  | Requires KVM
minikube | flannel | YES  | YES S8  | BLOCKED  | Requires KVM
kind     | kindnet | YES  | YES S9  | BLOCKED  | AVIC crash
kind     | cilium  | YES  | YES S7  | BLOCKED  | Uses kind-config-nocni.yaml
kind     | flannel | YES  | YES S8  | BLOCKED  | Uses kind-config-nocni.yaml
kind     | ovn     | NO   | NO      | NO       | OVN master CRDs broken on Kind K8s 1.31+

Storage: shared (all e2e, hardcoded) YES | NFS UNIMPLEMENTED | local/NBD NO E2E COVERAGE
Method: job YES (only implemented) | ssh REMOVED
