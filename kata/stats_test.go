package kata

import (
	"testing"
	"time"
)

func TestParseContainerMetricsCgroupV2(t *testing.T) {
	raw := `{
		"cpu": {
			"usage_usec": 500000,
			"user_usec": 300000,
			"system_usec": 200000,
			"nr_throttled": 5,
			"throttled_usec": 1000
		},
		"memory": {
			"usage": 10485760,
			"usage_limit": 20971520,
			"swap_usage": 0,
			"anon": 8388608,
			"file": 2097152
		}
	}`

	ts := time.Now()
	m, err := parseContainerMetrics(raw, ts)
	if err != nil {
		t.Fatal(err)
	}

	if m.CPUUsageNanos != 500000000 {
		t.Errorf("CPUUsageNanos = %d, want 500000000", m.CPUUsageNanos)
	}
	if m.CPUUserNanos != 300000000 {
		t.Errorf("CPUUserNanos = %d, want 300000000", m.CPUUserNanos)
	}
	if m.CPUSystemNanos != 200000000 {
		t.Errorf("CPUSystemNanos = %d, want 200000000", m.CPUSystemNanos)
	}
	if m.ThrottledPeriods != 5 {
		t.Errorf("ThrottledPeriods = %d, want 5", m.ThrottledPeriods)
	}
	if m.MemoryUsageBytes != 10485760 {
		t.Errorf("MemoryUsageBytes = %d, want 10485760", m.MemoryUsageBytes)
	}
	if m.MemoryRSSBytes != 8388608 {
		t.Errorf("MemoryRSSBytes = %d, want 8388608 (from anon)", m.MemoryRSSBytes)
	}
	if m.MemoryCacheBytes != 2097152 {
		t.Errorf("MemoryCacheBytes = %d, want 2097152 (from file)", m.MemoryCacheBytes)
	}
}

func TestParseContainerMetricsCgroupV1(t *testing.T) {
	raw := `{
		"cpu": {
			"usage": {
				"total": 1000000000,
				"user": 600000000,
				"kernel": 400000000
			},
			"throttling": {
				"throttled_periods": 10,
				"throttled_time": 50000000
			}
		},
		"memory": {
			"usage": {
				"usage": 52428800,
				"max": 104857600
			},
			"total_rss": 41943040,
			"total_cache": 10485760,
			"total_swap": 1048576,
			"total_mapped_file": 524288
		}
	}`

	m, err := parseContainerMetrics(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if m.CPUUsageNanos != 1000000000 {
		t.Errorf("CPUUsageNanos = %d, want 1000000000", m.CPUUsageNanos)
	}
	if m.ThrottledPeriods != 10 {
		t.Errorf("ThrottledPeriods = %d, want 10", m.ThrottledPeriods)
	}
	if m.ThrottledTimeNanos != 50000000 {
		t.Errorf("ThrottledTimeNanos = %d, want 50000000", m.ThrottledTimeNanos)
	}
	if m.MemoryUsageBytes != 52428800 {
		t.Errorf("MemoryUsageBytes = %d, want 52428800", m.MemoryUsageBytes)
	}
	if m.MemoryMaxUsageBytes != 104857600 {
		t.Errorf("MemoryMaxUsageBytes = %d, want 104857600", m.MemoryMaxUsageBytes)
	}
	if m.MemoryRSSBytes != 41943040 {
		t.Errorf("MemoryRSSBytes = %d, want 41943040", m.MemoryRSSBytes)
	}
	if m.MemoryMappedBytes != 524288 {
		t.Errorf("MemoryMappedBytes = %d, want 524288", m.MemoryMappedBytes)
	}
}

func TestResourceUsageCPUPercent(t *testing.T) {
	now := time.Now()
	prev := &containerMetrics{
		Timestamp:      now,
		CPUUsageNanos:  100000000,
		CPUUserNanos:   60000000,
		CPUSystemNanos: 40000000,
	}
	curr := &containerMetrics{
		Timestamp:      now.Add(time.Second),
		CPUUsageNanos:  200000000,
		CPUUserNanos:   120000000,
		CPUSystemNanos: 80000000,
	}

	usage := curr.ResourceUsage(prev)
	cpu := usage.ResourceUsage.CpuStats

	if cpu.Percent < 9.9 || cpu.Percent > 10.1 {
		t.Errorf("CPU Percent = %f, want ~10.0", cpu.Percent)
	}
	if cpu.UserMode < 5.9 || cpu.UserMode > 6.1 {
		t.Errorf("CPU UserMode = %f, want ~6.0", cpu.UserMode)
	}
	if cpu.SystemMode < 3.9 || cpu.SystemMode > 4.1 {
		t.Errorf("CPU SystemMode = %f, want ~4.0", cpu.SystemMode)
	}
}

func TestResourceUsageNoPrevious(t *testing.T) {
	m := &containerMetrics{
		Timestamp:        time.Now(),
		MemoryUsageBytes: 1024,
		MemoryRSSBytes:   512,
	}

	usage := m.ResourceUsage(nil)
	if usage.ResourceUsage.CpuStats.Percent != 0 {
		t.Error("CPU percent should be 0 without previous sample")
	}
	if usage.ResourceUsage.MemoryStats.RSS != 512 {
		t.Errorf("RSS = %d, want 512", usage.ResourceUsage.MemoryStats.RSS)
	}
}

func TestPercentEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		prev     uint64
		curr     uint64
		elapsed  float64
		want     float64
	}{
		{"current equals previous", 100000000, 100000000, 1e9, 0},
		{"current less than previous", 200000000, 100000000, 1e9, 0},
		{"zero elapsed", 100000000, 200000000, 0, 0},
		{"negative elapsed", 100000000, 200000000, -1, 0},
		{"normal", 100000000, 200000000, 1e9, 10.0},
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
