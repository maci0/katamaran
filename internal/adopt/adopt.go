// Package adopt defines the sandbox-adoption contract shared by the three
// components that participate in it:
//
//   - internal/migration re-parents the surviving destination QEMU into
//     CgroupRoot/<sandboxID> so it outlives the dest Job's container
//     (Approach E step 1).
//   - internal/controller stamps SandboxIDAnnotation on the adoption Pod,
//     pinning which surviving sandbox that Pod adopts.
//   - cmd/containerd-shim-katamaran-adopted-v2 reads the annotation from the
//     Pod's OCI bundle config.json and adopts the QEMU found under
//     CgroupRoot/<sandboxID>.
//
// These values are on-disk/wire contracts across separate binaries: they are
// defined here exactly once and must not be redefined locally. See
// docs/ROADMAP.md "Kata Sandbox Adoption -> Approach E" for design context.
package adopt

const (
	// CgroupRoot is the host cgroup directory where surviveContainerExit
	// moves the migrated QEMU (plus its KVM helper threads). The adopted
	// shim looks up surviving QEMUs under this path. Requires the host
	// cgroup root mounted into the containers at /sys (cgroup v2 unified
	// hierarchy).
	CgroupRoot = "/sys/fs/cgroup/katamaran-adopted"

	// SandboxIDAnnotation is the Pod annotation the controller sets to tell
	// the adopted shim which sandbox id under CgroupRoot to adopt. The shim
	// reads it from the pod's OCI bundle config.json.
	SandboxIDAnnotation = "katamaran.io/adopted-sandbox-id"

	// DefaultSandboxID is the well-known sandbox id used when no explicit id
	// exists: the cmdline-replay flow defaults the synthetic dest sandbox to
	// it, and the controller stamps it as the annotation value so the shim's
	// default matches what actually ran.
	DefaultSandboxID = "katamaran-dest"
)
