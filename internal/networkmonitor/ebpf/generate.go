package ebpf

// To regenerate eBPF artifacts, install llvm (brew install llvm) and uncomment the line below.
// Ensure you have the 'bpf_bpfel.o' and 'bpf_bpfel.go' files checked in.
//
// //go:generate go run github.com/cilium/ebpf/cmd/bpf2go -no-strip -cc clang -cflags $BPF_CFLAGS bpf ./c/egressd.c -- -I./c/include
