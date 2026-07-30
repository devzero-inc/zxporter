package nodemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testContainerID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestParseCPUStat_V2(t *testing.T) {
	// cgroup v2 cpu.stat: throttled_usec is already microseconds.
	content := `usage_usec 123456789
user_usec 90000000
system_usec 33456789
nr_periods 5000
nr_throttled 137
throttled_usec 4210000
`
	periods, throttledPeriods, throttledUsec := parseCPUStat(content)
	assert.Equal(t, int64(5000), periods)
	assert.Equal(t, int64(137), throttledPeriods)
	assert.Equal(t, int64(4210000), throttledUsec)
}

func TestParseCPUStat_V1_NanosConverted(t *testing.T) {
	// cgroup v1 cpu.stat: throttled_time is nanoseconds; convert to usec (/1000).
	content := `nr_periods 5000
nr_throttled 137
throttled_time 4210000000
`
	periods, throttledPeriods, throttledUsec := parseCPUStat(content)
	assert.Equal(t, int64(5000), periods)
	assert.Equal(t, int64(137), throttledPeriods)
	assert.Equal(t, int64(4210000), throttledUsec, "nanoseconds must be divided by 1000")
}

func TestParseCPUStat_Empty(t *testing.T) {
	p, tp, tu := parseCPUStat("")
	assert.Zero(t, p)
	assert.Zero(t, tp)
	assert.Zero(t, tu)
}

func TestParseMemoryEventsMax(t *testing.T) {
	// The decisive Guaranteed-memory ceiling counter (langfuse case).
	content := `low 0
high 0
max 42
oom 0
oom_kill 0
`
	assert.Equal(t, int64(42), parseMemoryEventsMax(content))
}

func TestParseMemoryEventsMax_Missing(t *testing.T) {
	assert.Zero(t, parseMemoryEventsMax("low 0\nhigh 0\n"))
}

func TestParsePSITotals_SomeAndFull(t *testing.T) {
	// memory.pressure exposes both some and full lines.
	content := `some avg10=0.00 avg60=0.10 avg300=0.05 total=987654
full avg10=0.00 avg60=0.05 avg300=0.02 total=123456
`
	some, full := parsePSITotals(content)
	assert.Equal(t, int64(987654), some)
	assert.Equal(t, int64(123456), full)
}

func TestParsePSITotals_SomeOnly(t *testing.T) {
	// cpu.pressure historically has no full line; full stays zero.
	content := `some avg10=1.00 avg60=0.50 avg300=0.25 total=555000
`
	some, full := parsePSITotals(content)
	assert.Equal(t, int64(555000), some)
	assert.Zero(t, full)
}

func TestParsePSITotals_Empty(t *testing.T) {
	some, full := parsePSITotals("")
	assert.Zero(t, some)
	assert.Zero(t, full)
}

func TestPSITotal_MissingField(t *testing.T) {
	assert.Zero(t, psiTotal([]string{"some", "avg10=0.00"}))
}

func TestParseSingleInt(t *testing.T) {
	assert.Equal(t, int64(99), parseSingleInt("99\n"))
	assert.Equal(t, int64(0), parseSingleInt(""))
	assert.Equal(t, int64(0), parseSingleInt("not-a-number"))
}

func TestParseCgroupDirContainerID(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		wantID string
		wantOK bool
	}{
		{"containerd scope", "cri-containerd-" + testContainerID + ".scope", testContainerID, true},
		{"docker scope", "docker-" + testContainerID + ".scope", testContainerID, true},
		{"crio scope", "crio-" + testContainerID + ".scope", testContainerID, true},
		{"bare id", testContainerID, testContainerID, true},
		{"pod slice not a container", "kubepods-besteffort-pod1234_5678.slice", "", false},
		{"short hex not a container", "abc123", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := parseCgroupDirContainerID(tc.base)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// writeFile writes content to dir/name, creating dir.
func writeCgroupFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func newTestReader(t *testing.T, root string, containerMap map[string]containerInfo) *CgroupReader {
	t.Helper()
	return &CgroupReader{
		root:  root,
		index: &PodContainerIndex{containerMap: containerMap},
		log:   testr.New(t),
	}
}

func TestCollect_V2_EndToEnd(t *testing.T) {
	root := t.TempDir()
	// Mark the hierarchy as unified v2.
	writeCgroupFile(t, root, "cgroup.controllers", "cpu memory io pids\n")

	scope := filepath.Join(root,
		"kubepods.slice",
		"kubepods-besteffort.slice",
		"kubepods-besteffort-pod1234_5678.slice",
		"cri-containerd-"+testContainerID+".scope",
	)
	writeCgroupFile(t, scope, "cpu.stat", "nr_periods 5000\nnr_throttled 137\nthrottled_usec 4210000\n")
	writeCgroupFile(t, scope, "memory.events", "low 0\nhigh 0\nmax 42\noom 0\noom_kill 0\n")
	writeCgroupFile(t, scope, "cpu.pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=555000\n")
	writeCgroupFile(t, scope, "memory.pressure",
		"some avg10=0.00 avg60=0.00 avg300=0.00 total=987654\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=123456\n")

	r := newTestReader(t, root, map[string]containerInfo{
		testContainerID: {Pod: "langfuse-web-0", Namespace: "prod", Container: "web"},
	})

	got := r.Collect()
	require.Len(t, got, 1)
	sig, ok := got["prod/langfuse-web-0/web"]
	require.True(t, ok, "signals must be keyed namespace/pod/container")

	assert.Equal(t, int64(5000), sig.CfsPeriods)
	assert.Equal(t, int64(137), sig.CfsThrottledPeriods)
	assert.Equal(t, int64(4210000), sig.CfsThrottledUsec)
	assert.Equal(t, int64(42), sig.MemoryEventsMax)
	assert.Equal(t, int64(555000), sig.CPUPressureSomeUsec)
	assert.Equal(t, int64(987654), sig.MemoryPressureSomeUsec)
	assert.Equal(t, int64(123456), sig.MemoryPressureFullUsec)
}

func TestCollect_V2_UnresolvedContainerSkipped(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, "cgroup.controllers", "cpu memory\n")
	scope := filepath.Join(root, "kubepods.slice", "cri-containerd-"+testContainerID+".scope")
	writeCgroupFile(t, scope, "cpu.stat", "nr_periods 1\nnr_throttled 0\nthrottled_usec 0\n")

	// Empty index: the container ID does not resolve, so nothing is emitted.
	r := newTestReader(t, root, map[string]containerInfo{})
	assert.Empty(t, r.Collect())
}

func TestCollect_V1_EndToEnd(t *testing.T) {
	root := t.TempDir()
	// No cgroup.controllers file => v1 path.

	cpuScope := filepath.Join(root, "cpu,cpuacct", "kubepods", "besteffort",
		"pod1234", "cri-containerd-"+testContainerID+".scope")
	writeCgroupFile(t, cpuScope, "cpu.stat", "nr_periods 900\nnr_throttled 12\nthrottled_time 3400000000\n")

	memScope := filepath.Join(root, "memory", "kubepods", "besteffort",
		"pod1234", "cri-containerd-"+testContainerID+".scope")
	writeCgroupFile(t, memScope, "memory.failcnt", "7\n")

	r := newTestReader(t, root, map[string]containerInfo{
		testContainerID: {Pod: "app-0", Namespace: "default", Container: "app"},
	})

	got := r.Collect()
	require.Len(t, got, 1)
	sig, ok := got["default/app-0/app"]
	require.True(t, ok)

	assert.Equal(t, int64(900), sig.CfsPeriods)
	assert.Equal(t, int64(12), sig.CfsThrottledPeriods)
	assert.Equal(t, int64(3400000), sig.CfsThrottledUsec, "v1 throttled_time ns must convert to usec")
	assert.Equal(t, int64(7), sig.MemoryEventsMax, "v1 memory.failcnt maps to MemoryEventsMax")
	// PSI does not exist on v1.
	assert.Zero(t, sig.CPUPressureSomeUsec)
	assert.Zero(t, sig.MemoryPressureSomeUsec)
	assert.Zero(t, sig.MemoryPressureFullUsec)
}

func TestCollect_NilSafe(t *testing.T) {
	var r *CgroupReader
	assert.Nil(t, r.Collect(), "nil reader")

	r = &CgroupReader{root: "/sys/fs/cgroup", index: nil, log: testr.New(t)}
	assert.Nil(t, r.Collect(), "nil index")
}

func TestCollect_MissingRoot(t *testing.T) {
	r := newTestReader(t, filepath.Join(t.TempDir(), "does-not-exist"),
		map[string]containerInfo{testContainerID: {Pod: "p", Namespace: "n", Container: "c"}})
	assert.Nil(t, r.Collect())
}
