package nodemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitJavaOpts(t *testing.T) {
	assert.Equal(t, []string{"-Xms48m", "-Xmx160m", "-XX:MaxRAMPercentage=65", "-Dfrom=tooloptions"}, splitJavaOpts("-Xms48m -Xmx160m -XX:MaxRAMPercentage=65 -Dfrom=tooloptions"))
	assert.Equal(t, []string{"-Dfoo=bar baz", "-Xmx256m"}, splitJavaOpts("-Dfoo='bar baz' -Xmx256m"))
	assert.Equal(t, []string{"-Dfoo=bar baz", "-Xmx256m"}, splitJavaOpts("-Dfoo=\"bar baz\" -Xmx256m"))
}

func TestReadJVMFlagsFromProcEnviron(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "environ")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	// NUL-separated key=value pairs.
	_, err = f.WriteString("PATH=/usr/bin\x00JAVA_TOOL_OPTIONS=-Xms48m -Xmx160m -XX:MaxRAMPercentage=65 -Dfrom=tooloptions\x00OTHER=x\x00")
	require.NoError(t, err)
	require.NoError(t, f.Sync())

	env := readEnvVars(f.Name(), "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JAVA_OPTS")
	flags, _, _ := ParseJVMFlagsWithSources("java", env)
	parsed := flags
	require.NotNil(t, parsed.XmsBytes)
	require.NotNil(t, parsed.XmxBytes)
	require.NotNil(t, parsed.MaxRamPercentage)
	assert.Equal(t, int64(48*1024*1024), *parsed.XmsBytes)
	assert.Equal(t, int64(160*1024*1024), *parsed.XmxBytes)
	assert.Equal(t, 65.0, *parsed.MaxRamPercentage)
}

func TestParseCgroupContainerID(t *testing.T) {
	const id64 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	tests := []struct {
		name    string
		content string
		wantID  string
		wantOK  bool
	}{
		{
			name: "containerd cgroupv1",
			content: "12:cpuset:/kubepods/besteffort/pod1234/cri-containerd-" + id64 + ".scope\n" +
				"0::/kubepods/besteffort/pod1234/cri-containerd-" + id64 + ".scope\n",
			wantID: id64,
			wantOK: true,
		},
		{
			name:    "docker scope",
			content: "12:cpuset:/kubepods/docker-" + id64 + ".scope\n",
			wantID:  id64,
			wantOK:  true,
		},
		{
			name:    "bare containerd scope",
			content: "12:cpuset:/kubepods/containerd-" + id64 + ".scope\n",
			wantID:  id64,
			wantOK:  true,
		},
		{
			name:    "crio scope",
			content: "12:cpuset:/kubepods/crio-" + id64 + ".scope\n",
			wantID:  id64,
			wantOK:  true,
		},
		{
			name:    "non-k8s container",
			content: "12:cpuset:/user.slice\n0::/user.slice/user-1000.slice\n",
			wantID:  "",
			wantOK:  false,
		},
		{
			name:    "empty content",
			content: "",
			wantID:  "",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := parseCgroupContainerID(tc.content)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantID, id)
			}
		})
	}
}

func TestParseNSpid(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantPid int
		wantOK  bool
	}{
		{
			name:    "nested namespace - takes last value",
			content: "Name:\tjava\nPid:\t12345\nNSpid:\t12345\t67\nTgid:\t12345\n",
			wantPid: 67,
			wantOK:  true,
		},
		{
			name:    "single namespace",
			content: "Name:\tjava\nNSpid:\t42\nTgid:\t42\n",
			wantPid: 42,
			wantOK:  true,
		},
		{
			name:    "three levels of nesting",
			content: "NSpid:\t1000\t500\t1\n",
			wantPid: 1,
			wantOK:  true,
		},
		{
			name:    "missing NSpid line",
			content: "Name:\tjava\nPid:\t12345\n",
			wantPid: 0,
			wantOK:  false,
		},
		{
			name:    "empty content",
			content: "",
			wantPid: 0,
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pid, ok := parseNSpid(tc.content)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantPid, pid)
			}
		})
	}
}

func TestParseJVMFlags(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		xms     *int64
		xmx     *int64
		maxRam  *float64
		useCS   *bool
	}{
		{
			name:    "basic heap flags megabytes and gigabytes",
			cmdline: "java -Xms256m -Xmx2g -jar app.jar",
			xms:     jvmIntPtr(256 * 1024 * 1024),
			xmx:     jvmIntPtr(2 * 1024 * 1024 * 1024),
		},
		{
			name:    "container support enabled with MaxRAMPercentage",
			cmdline: "java -XX:MaxRAMPercentage=75.0 -XX:+UseContainerSupport -jar app.jar",
			maxRam:  jvmF64Ptr(75.0),
			useCS:   jvmBoolPtr(true),
		},
		{
			name:    "container support disabled",
			cmdline: "java -XX:-UseContainerSupport -jar app.jar",
			useCS:   jvmBoolPtr(false),
		},
		{
			name:    "kilobytes",
			cmdline: "java -Xms512k -Xmx4g",
			xms:     jvmIntPtr(512 * 1024),
			xmx:     jvmIntPtr(4 * 1024 * 1024 * 1024),
		},
		{
			name:    "uppercase suffix",
			cmdline: "java -Xms128M -Xmx1G",
			xms:     jvmIntPtr(128 * 1024 * 1024),
			xmx:     jvmIntPtr(1 * 1024 * 1024 * 1024),
		},
		{
			name:    "no JVM flags",
			cmdline: "java -jar app.jar",
		},
		{
			name:    "full path java binary",
			cmdline: "/usr/lib/jvm/java-21/bin/java -Xmx512m -jar app.jar",
			xmx:     jvmIntPtr(512 * 1024 * 1024),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flags := ParseJVMFlags(tc.cmdline)
			assert.Equal(t, tc.xms, flags.XmsBytes, "XmsBytes")
			assert.Equal(t, tc.xmx, flags.XmxBytes, "XmxBytes")
			assert.Equal(t, tc.maxRam, flags.MaxRamPercentage, "MaxRamPercentage")
			assert.Equal(t, tc.useCS, flags.UseContainerSupport, "UseContainerSupport")
		})
	}
}

func TestParseMemSize(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"256m", 256 * 1024 * 1024, false},
		{"2g", 2 * 1024 * 1024 * 1024, false},
		{"512k", 512 * 1024, false},
		{"1024", 1024, false},
		{"4G", 4 * 1024 * 1024 * 1024, false},
		{"128M", 128 * 1024 * 1024, false},
		{"1K", 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.5g", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			v, err := parseMemSize(tc.input)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, v)
			}
		})
	}
}

func TestIsJavaProcess(t *testing.T) {
	tests := []struct {
		comm    string
		cmdline string
		want    bool
	}{
		{"java", "", true},
		{"java", "/usr/bin/java -jar app.jar", true},
		{"", "/usr/bin/java -jar app.jar", true},
		{"", "/usr/lib/jvm/java-21/bin/java -Xmx512m", true},
		{"python3", "/usr/bin/python3 app.py", false},
		{"", "", false},
		{"javac", "/usr/bin/javac Main.java", false},
	}

	for _, tc := range tests {
		t.Run(tc.comm+"|"+tc.cmdline, func(t *testing.T) {
			assert.Equal(t, tc.want, isJavaProcess(tc.comm, tc.cmdline))
		})
	}
}

func TestStripContainerIDScheme(t *testing.T) {
	assert.Equal(t, "abc123", stripContainerIDScheme("containerd://abc123"))
	assert.Equal(t, "abc123", stripContainerIDScheme("docker://abc123"))
	assert.Equal(t, "abc123", stripContainerIDScheme("abc123"))
	assert.Equal(t, "", stripContainerIDScheme(""))
	assert.Equal(t, "abc123", stripContainerIDScheme("cri-o://abc123"))
}

// jvmIntPtr returns a pointer to the given int64, for use in test assertions.
func jvmIntPtr(v int64) *int64 { return &v }

// jvmF64Ptr returns a pointer to the given float64, for use in test assertions.
func jvmF64Ptr(v float64) *float64 { return &v }

// jvmBoolPtr returns a pointer to the given bool, for use in test assertions.
func jvmBoolPtr(v bool) *bool { return &v }
