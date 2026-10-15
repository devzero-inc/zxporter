//go:build amd64 || arm64

package socketprobe

import (
	"testing"

	bpf "github.com/cilium/ebpf"
)

// The loader looks each program up in the object file by name and attaches it to a
// kernel function by name, and neither lookup is checked by the compiler: the object is
// a checked-in build artefact, so a program added to c/socketprobe.c without
// regenerating, or renamed on one side only, builds cleanly and fails at load time on
// the node.
func TestObjectContainsEveryProgramTheLoaderAttaches(t *testing.T) {
	spec, err := loadSocketprobe()
	if err != nil {
		t.Fatalf("loading the embedded object: %v", err)
	}

	// The receive path is deliberately compiled twice, and only one variant is loaded
	// on any given kernel, so both have to be present.
	want := map[string]struct {
		typ      bpf.ProgramType
		attachTo string
	}{
		"sr_tcp_sendmsg":        {bpf.Kprobe, "tcp_sendmsg"},
		"sr_tcp_recvmsg_fentry": {bpf.Tracing, "tcp_recvmsg"},
		"sr_tcp_recvmsg_fexit":  {bpf.Tracing, "tcp_recvmsg"},
		"sr_tcp_recvmsg_entry":  {bpf.Kprobe, "tcp_recvmsg"},
		"sr_tcp_recvmsg_exit":   {bpf.Kprobe, "tcp_recvmsg"},
		"sr_tcp_close":          {bpf.Kprobe, "tcp_close"},
		// tcp_done is the kernel ending a connection by itself. Without it the close
		// arrives from tcp_close with the local port already released, under a key that
		// matches nothing, and the connection leaks until the idle sweep. See the
		// comment on the program in c/socketprobe.c.
		"sr_tcp_done": {bpf.Kprobe, "tcp_done"},
	}

	for name, w := range want {
		prog, ok := spec.Programs[name]
		if !ok {
			t.Errorf("program %s is missing from the object; regenerate with bpf2go (see generate.go)", name)
			continue
		}
		if prog.Type != w.typ {
			t.Errorf("program %s has type %v, want %v", name, prog.Type, w.typ)
		}
		if prog.AttachTo != w.attachTo {
			t.Errorf("program %s attaches to %q, want %q", name, prog.AttachTo, w.attachTo)
		}
	}

	for name := range spec.Programs {
		if _, expected := want[name]; !expected {
			t.Errorf("object contains program %s, which the loader never attaches", name)
		}
	}
}
