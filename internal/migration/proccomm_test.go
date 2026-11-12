package migration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKVMHelperName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		comm    string
		suffix  string
		want    string
		matched bool
	}{
		{"helper", "kvm-nx-lpage-recovery-42\n", "-42", "kvm-nx-lpage-recovery-42", true},
		{"longer pid", "kvm-nx-lpage-recovery-421\n", "-42", "", false},
		{"slash separator", "kvm-pit/42\n", "-42", "", false},
		{"not kvm", "bash\n", "-42", "", false},
		{"no newline", "kvm-x-5", "-5", "kvm-x-5", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := kvmHelperName([]byte(tc.comm), []byte(tc.suffix))
			if ok != tc.matched || string(got) != tc.want {
				t.Fatalf("kvmHelperName(%q, %q) = %q, %v; want %q, %v", tc.comm, tc.suffix, got, ok, tc.want, tc.matched)
			}
		})
	}
}

func TestReadProcComm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "comm")
	const content = "kvm-nx-lpage-recovery-7\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	got, ok := readProcComm(path, buf)
	if !ok || string(got) != content {
		t.Fatalf("readProcComm = %q, %v; want %q, true", got, ok, content)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readProcComm(path, buf); ok {
		t.Fatal("empty comm file reported ok")
	}
	if _, ok := readProcComm(filepath.Join(dir, "missing"), buf); ok {
		t.Fatal("missing comm file reported ok")
	}
}
