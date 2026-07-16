package collector

import "testing"

// TestDeriveMemoryCacheSplit covers the active/inactive cache derivation and its
// underflow clamping.
//
//	inactive = max(0, usage - workingSet)
//	active   = max(0, cache - inactive)
func TestDeriveMemoryCacheSplit(t *testing.T) {
	tests := []struct {
		name         string
		workingSet   uint64
		usage        uint64
		cache        uint64
		wantActive   int64
		wantInactive int64
	}{
		{
			name:         "typical split",
			workingSet:   100,
			usage:        180, // inactive = 80
			cache:        120, // active = 120 - 80 = 40
			wantActive:   40,
			wantInactive: 80,
		},
		{
			name:         "usage below working set clamps inactive to 0",
			workingSet:   200,
			usage:        150, // usage < workingSet → inactive clamps to 0
			cache:        90,  // active = 90 - 0 = 90
			wantActive:   90,
			wantInactive: 0,
		},
		{
			name:         "cache below inactive clamps active to 0",
			workingSet:   100,
			usage:        300, // inactive = 200
			cache:        50,  // cache < inactive → active clamps to 0
			wantActive:   0,
			wantInactive: 200,
		},
		{
			name:         "all zero",
			workingSet:   0,
			usage:        0,
			cache:        0,
			wantActive:   0,
			wantInactive: 0,
		},
		{
			name:         "no cache but has inactive",
			workingSet:   100,
			usage:        160, // inactive = 60
			cache:        0,   // active = 0
			wantActive:   0,
			wantInactive: 60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, inactive := deriveMemoryCacheSplit(tt.workingSet, tt.usage, tt.cache)
			if active != tt.wantActive {
				t.Errorf("active = %d, want %d", active, tt.wantActive)
			}
			if inactive != tt.wantInactive {
				t.Errorf("inactive = %d, want %d", inactive, tt.wantInactive)
			}
		})
	}
}

// TestGetContainerMemoryBreakdown covers cache hits and misses for the nodemon
// memory-breakdown lookup used by processContainerMetrics.
func TestGetContainerMemoryBreakdown(t *testing.T) {
	c := &ContainerResourceCollector{
		nodemonContainerMetricsCache: map[string][]UnifiedContainerMetric{
			"ns/pod": {
				{
					Container:        "app",
					MemoryWorkingSet: 100,
					MemoryUsageBytes: 180,
					MemoryCacheBytes: 120,
					MemorySwapBytes:  16,
				},
			},
		},
	}

	t.Run("hit", func(t *testing.T) {
		got := c.getContainerMemoryBreakdown("ns", "pod", "app")
		if !got.found {
			t.Fatal("expected found=true")
		}
		if got.workingSet != 100 || got.usage != 180 || got.cache != 120 || got.swap != 16 {
			t.Errorf("unexpected breakdown: %+v", got)
		}
	})

	t.Run("container miss", func(t *testing.T) {
		if got := c.getContainerMemoryBreakdown("ns", "pod", "missing"); got.found {
			t.Errorf("expected found=false for unknown container, got %+v", got)
		}
	})

	t.Run("pod miss", func(t *testing.T) {
		if got := c.getContainerMemoryBreakdown("ns", "other", "app"); got.found {
			t.Errorf("expected found=false for unknown pod, got %+v", got)
		}
	})

	t.Run("nil cache", func(t *testing.T) {
		empty := &ContainerResourceCollector{}
		if got := empty.getContainerMemoryBreakdown("ns", "pod", "app"); got.found {
			t.Errorf("expected found=false for nil cache, got %+v", got)
		}
	})
}
