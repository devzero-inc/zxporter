package nodemon

import (
	"encoding/json"
	"testing"
	"time"
)

const storageSummaryJSON = `{
  "node": {
    "nodeName": "node-a",
    "cpu": {"time": "2026-08-19T16:20:03Z", "usageNanoCores": 1500000000},
    "memory": {"workingSetBytes": 2147483648},
    "fs": {
      "time": "2026-08-19T16:20:03Z",
      "availableBytes": 94501658624,
      "capacityBytes": 107294470144,
      "usedBytes": 12792811520,
      "inodesFree": 52254168,
      "inodes": 52422640,
      "inodesUsed": 168472
    },
    "runtime": {
      "imageFs": {
        "time": "2026-08-19T16:20:01Z",
        "availableBytes": 94501658624,
        "capacityBytes": 107294470144,
        "usedBytes": 6701690880,
        "inodesFree": 52254168,
        "inodes": 52422640,
        "inodesUsed": 78769
      }
    }
  },
  "pods": []
}`

func TestBuildNodeMetrics_Storage(t *testing.T) {
	var stats StatsSummary
	if err := json.Unmarshal([]byte(storageSummaryJSON), &stats); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	u := &UnifiedExporter{nodeName: "node-a", nodeNetRates: NewRateCalculator()}
	got := u.buildNodeMetrics(&stats, nil, time.Now())

	if got.NodefsUsedBytes != 12792811520 {
		t.Errorf("NodefsUsedBytes = %d, want 12792811520", got.NodefsUsedBytes)
	}
	if got.NodefsCapacityBytes != 107294470144 {
		t.Errorf("NodefsCapacityBytes = %d, want 107294470144", got.NodefsCapacityBytes)
	}
	if got.NodefsAvailableBytes != 94501658624 {
		t.Errorf("NodefsAvailableBytes = %d, want 94501658624", got.NodefsAvailableBytes)
	}
	if got.NodefsInodesUsed != 168472 {
		t.Errorf("NodefsInodesUsed = %d, want 168472", got.NodefsInodesUsed)
	}
	if got.NodefsInodesTotal != 52422640 {
		t.Errorf("NodefsInodesTotal = %d, want 52422640", got.NodefsInodesTotal)
	}
	if got.ImagefsUsedBytes != 6701690880 {
		t.Errorf("ImagefsUsedBytes = %d, want 6701690880", got.ImagefsUsedBytes)
	}
	if !got.ImagefsSharedDevice {
		t.Error("ImagefsSharedDevice = false, want true (identical capacity and inode totals)")
	}
}

// A node with no fs section at all must yield zeros, not a nil-pointer panic.
func TestBuildNodeMetrics_StorageAbsent(t *testing.T) {
	var stats StatsSummary
	const noFS = `{"node": {"nodeName": "node-a", "cpu": {"usageNanoCores": 1}}, "pods": []}`
	if err := json.Unmarshal([]byte(noFS), &stats); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	u := &UnifiedExporter{nodeName: "node-a", nodeNetRates: NewRateCalculator()}
	got := u.buildNodeMetrics(&stats, nil, time.Now())

	if got.NodefsCapacityBytes != 0 || got.ImagefsUsedBytes != 0 || got.ImagefsSharedDevice {
		t.Errorf("expected zero-valued storage for a summary with no fs section, got %+v", got)
	}
}

func TestImagefsSharesDeviceWithNodefs(t *testing.T) {
	u64 := func(v uint64) *uint64 { return &v }

	tests := []struct {
		name            string
		nodefs, imagefs *FsStats
		want            bool
	}{
		{
			name:    "same capacity and inodes is shared",
			nodefs:  &FsStats{CapacityBytes: u64(100), Inodes: u64(50)},
			imagefs: &FsStats{CapacityBytes: u64(100), Inodes: u64(50)},
			want:    true,
		},
		{
			name:    "different capacity is separate",
			nodefs:  &FsStats{CapacityBytes: u64(100), Inodes: u64(50)},
			imagefs: &FsStats{CapacityBytes: u64(200), Inodes: u64(50)},
			want:    false,
		},
		{
			name:    "same capacity but different inodes is separate",
			nodefs:  &FsStats{CapacityBytes: u64(100), Inodes: u64(50)},
			imagefs: &FsStats{CapacityBytes: u64(100), Inodes: u64(70)},
			want:    false,
		},
		{
			name:    "missing imagefs is not shared",
			nodefs:  &FsStats{CapacityBytes: u64(100), Inodes: u64(50)},
			imagefs: nil,
			want:    false,
		},
		{
			name:    "zero capacity is not shared even when equal",
			nodefs:  &FsStats{CapacityBytes: u64(0), Inodes: u64(0)},
			imagefs: &FsStats{CapacityBytes: u64(0), Inodes: u64(0)},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imagefsSharesDeviceWithNodefs(tt.nodefs, tt.imagefs); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
