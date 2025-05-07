package version

import (
	"fmt"
	"runtime"
)

const (
	versionNamespace     = "devzero-zxporter"
	versionConfigMapName = "zxporter-version"
)

type Info struct {
	Major        string `json:"major"`
	Minor        string `json:"minor"`
	Patch        string `json:"patch"`
	GitCommit    string `json:"gitCommit"`
	GitTreeState string `json:"gitTreeState"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

func (info Info) String() string {
	return fmt.Sprintf("%s.%s.%s", info.Major, info.Minor, info.Patch)
}

var (
	major        string
	minor        string
	patch        string
	gitCommit    string
	gitTreeState string
	buildDate    string
)

var Get = func() Info {
	return Info{
		Major:        major,
		Minor:        minor,
		Patch:        patch,
		GitCommit:    gitCommit,
		GitTreeState: gitTreeState,
		BuildDate:    buildDate,
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}
