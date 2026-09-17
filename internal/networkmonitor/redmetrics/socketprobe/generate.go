package socketprobe

// The compiled objects and their loader are checked in, so an ordinary build and
// an ordinary `go generate ./...` need no clang at all. The directive below is
// deliberately inert, matching internal/networkmonitor/ebpf: regenerating needs a
// clang that can target BPF, and the one a Mac has by default cannot, so a live
// directive would break `go generate ./...` for everyone on a Mac.
//
// That is a statement about Apple's clang and not about the machine. A Homebrew LLVM
// targets BPF fine, so on a Mac with `brew install llvm` this runs in place, from
// this directory:
//
//	GOPACKAGE=socketprobe go run github.com/cilium/ebpf/cmd/bpf2go -no-strip \
//	  -target amd64,arm64 -cc /opt/homebrew/opt/llvm/bin/clang \
//	  -cflags "-O2 -g -Wall -I../../ebpf/c/include" socketprobe ./c/socketprobe.c
//
// Check the toolchain first with `llc --version`: the target list has to include
// bpfel and bpfeb. Without such a clang, run the same bpf2go inside a container that
// has one:
//
//	docker run --rm -v "$(git rev-parse --show-toplevel)/operators/zxporter":/src \
//	  -w /src/internal/networkmonitor/redmetrics/socketprobe -e GOPACKAGE=socketprobe \
//	  <bpf-builder-image> go run github.com/cilium/ebpf/cmd/bpf2go -no-strip \
//	  -target amd64,arm64 -cc clang -cflags "-O2 -g -Wall -I../../ebpf/c/include" \
//	  socketprobe ./c/socketprobe.c
//
// Regenerating is not optional when a program is added or renamed. The loader reads
// each program out of the object by its section name, so a hand-edited
// socketprobe_*_bpfel.go naming a program the object does not contain fails at load
// time on the node, not at compile time here.
//
// The C file asserts its own record offsets, so a layout change fails that
// compile rather than shifting what the Go decoder reads.
//
// //go:generate go run github.com/cilium/ebpf/cmd/bpf2go -no-strip -target amd64,arm64 -cc clang -cflags "-O2 -g -Wall -I../../ebpf/c/include" socketprobe ./c/socketprobe.c
