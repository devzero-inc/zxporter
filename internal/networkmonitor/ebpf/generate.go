package ebpf

// To regenerate eBPF artifacts, install llvm (brew install llvm) and uncomment the line below.
// Ensure you have the 'bpf_bpfel.o' and 'bpf_bpfel.go' files checked in.
//
////go:generate go run github.com/cilium/ebpf/cmd/bpf2go -no-strip -cc /opt/homebrew/opt/llvm/bin/clang -cflags "$BPF_CFLAGS -D__TARGET_ARCH_x86" bpf ./c/tracee.c -- -I./c/include
