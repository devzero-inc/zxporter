package version

import (
	"context"
	"fmt"
	"runtime"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

func CreateVersionConfigMap(logger logr.Logger, ctx context.Context, K8sClient *kubernetes.Clientset) {
	versionInfo := Get()

	versionConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      versionConfigMapName,
			Namespace: versionNamespace,
		},
		Data: map[string]string{
			"version":        versionInfo.String(),
			"git_commit":     versionInfo.GitCommit,
			"git_tree_state": versionInfo.GitTreeState,
			"build_date":     versionInfo.BuildDate,
			"go_version":     versionInfo.GoVersion,
			"compiler":       versionInfo.Compiler,
			"platform":       versionInfo.Platform,
		},
	}

	// Try to get the existing ConfigMap
	existingConfigMap, err := K8sClient.CoreV1().ConfigMaps(versionNamespace).Get(ctx, versionConfigMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating zxporter version ConfigMap",
				"version", versionInfo.String(),
				"commit", versionInfo.GitCommit)

			_, err = K8sClient.CoreV1().ConfigMaps(versionNamespace).Create(
				ctx,
				versionConfigMap,
				metav1.CreateOptions{},
			)
			if err != nil {
				logger.Error(err, "Failed to create version ConfigMap")
			}
		} else {
			logger.Error(err, "Error checking for existing version ConfigMap")
		}
	} else {
		if existingConfigMap.Data["version"] != versionInfo.String() ||
			existingConfigMap.Data["git_commit"] != versionInfo.GitCommit {

			logger.Info("Updating zxporter version ConfigMap",
				"old_version", existingConfigMap.Data["version"],
				"new_version", versionInfo.String(),
				"old_commit", existingConfigMap.Data["git_commit"],
				"new_commit", versionInfo.GitCommit)

			existingConfigMap.Data = versionConfigMap.Data

			_, err = K8sClient.CoreV1().ConfigMaps(versionNamespace).Update(
				ctx,
				existingConfigMap,
				metav1.UpdateOptions{},
			)
			if err != nil {
				logger.Error(err, "Failed to update version ConfigMap")
			}
		} else {
			logger.Info("zxporter version ConfigMap is up to date",
				"version", versionInfo.String())
		}
	}
}
