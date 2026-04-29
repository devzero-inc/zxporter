package nodemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

const fullStatsSummaryJSON = `{
  "node": {
    "nodeName": "test-node",
    "network": {
      "interfaces": [
        {"name": "eth0", "rxBytes": 1000, "txBytes": 2000}
      ]
    }
  },
  "pods": [
    {
      "podRef": {
        "name": "my-pod",
        "namespace": "default"
      },
      "containers": [
        {
          "name": "my-container",
          "cpu": {
            "time": "2024-01-01T00:00:00Z",
            "usageNanoCores": 500000,
            "usageCoreNanoSeconds": 1234567890
          },
          "memory": {
            "time": "2024-01-01T00:00:00Z",
            "usageBytes": 104857600,
            "workingSetBytes": 52428800,
            "rssBytes": 26214400,
            "availableBytes": 524288000,
            "pageFaults": 10,
            "majorPageFaults": 0
          }
        }
      ],
      "network": {
        "interfaces": [
          {"name": "eth0", "rxBytes": 300, "txBytes": 400}
        ]
      },
      "volume": [
        {
          "name": "my-pvc",
          "pvcRef": {
            "name": "my-claim",
            "namespace": "default"
          },
          "usedBytes": 1073741824,
          "capacityBytes": 10737418240,
          "availableBytes": 9663676416
        }
      ]
    }
  ]
}`

func TestStatsPoller_ParsesKubeletResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/summary" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fullStatsSummaryJSON))
	}))
	defer srv.Close()

	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	poller := nodemon.NewStatsPoller(srv.URL, srv.Client(), log)

	summary, err := poller.Poll(context.Background())

	r := require.New(t)
	r.NoError(err)
	r.NotNil(summary)

	// Node
	r.Equal("test-node", summary.Node.NodeName)

	// Pod
	r.Len(summary.Pods, 1)
	pod := summary.Pods[0]
	r.Equal("my-pod", pod.PodRef.Name)
	r.Equal("default", pod.PodRef.Namespace)

	// Container CPU
	r.Len(pod.Containers, 1)
	c := pod.Containers[0]
	r.Equal("my-container", c.Name)
	r.NotNil(c.CPU.UsageNanoCores)
	r.Equal(uint64(500000), *c.CPU.UsageNanoCores)
	r.NotNil(c.CPU.UsageCoreNanoSeconds)
	r.Equal(uint64(1234567890), *c.CPU.UsageCoreNanoSeconds)

	// Container Memory
	r.NotNil(c.Memory.UsageBytes)
	r.Equal(uint64(104857600), *c.Memory.UsageBytes)
	r.NotNil(c.Memory.WorkingSetBytes)
	r.Equal(uint64(52428800), *c.Memory.WorkingSetBytes)
	r.NotNil(c.Memory.RSSBytes)
	r.Equal(uint64(26214400), *c.Memory.RSSBytes)

	// Pod Network
	r.Len(pod.Network.Interfaces, 1)
	iface := pod.Network.Interfaces[0]
	r.Equal("eth0", iface.Name)
	r.NotNil(iface.RxBytes)
	r.Equal(uint64(300), *iface.RxBytes)
	r.NotNil(iface.TxBytes)
	r.Equal(uint64(400), *iface.TxBytes)

	// Volume
	r.Len(pod.VolumeStats, 1)
	vol := pod.VolumeStats[0]
	r.Equal("my-pvc", vol.Name)
	r.NotNil(vol.PVCRef)
	r.Equal("my-claim", vol.PVCRef.Name)
	r.Equal("default", vol.PVCRef.Namespace)
	r.NotNil(vol.UsedBytes)
	r.Equal(uint64(1073741824), *vol.UsedBytes)
	r.NotNil(vol.CapacityBytes)
	r.Equal(uint64(10737418240), *vol.CapacityBytes)
}

func TestStatsPoller_HandlesEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"node":{"nodeName":"empty-node","network":{}},"pods":[]}`))
	}))
	defer srv.Close()

	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	poller := nodemon.NewStatsPoller(srv.URL, srv.Client(), log)

	summary, err := poller.Poll(context.Background())

	r := require.New(t)
	r.NoError(err)
	r.NotNil(summary)
	r.Empty(summary.Pods)
	r.Equal("empty-node", summary.Node.NodeName)
}

func TestStatsPoller_HandlesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	poller := nodemon.NewStatsPoller(srv.URL, srv.Client(), log)

	summary, err := poller.Poll(context.Background())

	r := require.New(t)
	r.Error(err)
	r.Nil(summary)
}
