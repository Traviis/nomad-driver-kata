package kata

import (
	"testing"
	"time"
)

func TestResourceUsageCPUPercent(t *testing.T) {
	ts1 := time.Now()
	ts2 := ts1.Add(time.Second)

	prev := &containerMetrics{
		Timestamp:      ts1,
		CPUUsageNanos:  100_000_000,
		CPUUserNanos:   60_000_000,
		CPUSystemNanos: 40_000_000,
	}
	curr := &containerMetrics{
		Timestamp:      ts2,
		CPUUsageNanos:  200_000_000,
		CPUUserNanos:   130_000_000,
		CPUSystemNanos: 70_000_000,
	}

	usage := curr.ResourceUsage(prev)
	cpu := usage.ResourceUsage.CpuStats
	if cpu.Percent < 9.9 || cpu.Percent > 10.1 {
		t.Errorf("Percent = %f, want ~10.0", cpu.Percent)
	}
	if cpu.UserMode < 6.9 || cpu.UserMode > 7.1 {
		t.Errorf("UserMode = %f, want ~7.0", cpu.UserMode)
	}
	if cpu.SystemMode < 2.9 || cpu.SystemMode > 3.1 {
		t.Errorf("SystemMode = %f, want ~3.0", cpu.SystemMode)
	}
}

func TestResourceUsageNoPrevious(t *testing.T) {
	m := &containerMetrics{
		Timestamp:        time.Now(),
		CPUUsageNanos:    500_000_000,
		MemoryUsageBytes: 1024 * 1024,
		MemoryRSSBytes:   512 * 1024,
	}

	usage := m.ResourceUsage(nil)
	cpu := usage.ResourceUsage.CpuStats
	if cpu.Percent != 0 {
		t.Errorf("Percent = %f, want 0 with nil previous", cpu.Percent)
	}

	mem := usage.ResourceUsage.MemoryStats
	if mem.RSS != 512*1024 {
		t.Errorf("RSS = %d, want %d", mem.RSS, 512*1024)
	}
	if mem.Usage != 1024*1024 {
		t.Errorf("Usage = %d, want %d", mem.Usage, 1024*1024)
	}
}

func TestPercentEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		prev    uint64
		curr    uint64
		elapsed float64
		want    float64
	}{
		{"current equals previous", 100, 100, 1e9, 0},
		{"current less than previous", 200, 100, 1e9, 0},
		{"zero elapsed", 100, 200, 0, 0},
		{"negative elapsed", 100, 200, -1e9, 0},
		{"normal", 0, 1e9, 1e9, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percent(tt.prev, tt.curr, tt.elapsed)
			if got != tt.want {
				t.Errorf("percent(%d, %d, %f) = %f, want %f", tt.prev, tt.curr, tt.elapsed, got, tt.want)
			}
		})
	}
}
