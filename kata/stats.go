package kata

import (
	"fmt"
	"time"

	"github.com/containerd/containerd/api/types"
	v2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/typeurl/v2"
	"github.com/hashicorp/nomad/plugins/drivers"
)

var (
	measuredMemoryStats = []string{"Usage", "Max Usage", "RSS", "Cache", "Swap", "Mapped File"}
	measuredCPUStats    = []string{"Percent", "System Mode", "User Mode", "Throttled Periods", "Throttled Time"}
)

type containerMetrics struct {
	Timestamp time.Time

	CPUUsageNanos       uint64
	CPUUserNanos        uint64
	CPUSystemNanos      uint64
	ThrottledPeriods    uint64
	ThrottledTimeNanos  uint64
	MemoryUsageBytes    uint64
	MemoryMaxUsageBytes uint64
	MemoryRSSBytes      uint64
	MemoryCacheBytes    uint64
	MemorySwapBytes     uint64
	MemoryMappedBytes   uint64
}

func parseMetricProto(metric *types.Metric) (*containerMetrics, error) {
	data, err := typeurl.UnmarshalAny(metric.Data)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling metrics: %w", err)
	}

	m := &containerMetrics{Timestamp: time.Now().UTC()}

	switch stats := data.(type) {
	case *v2.Metrics:
		if stats.CPU != nil {
			m.CPUUsageNanos = stats.CPU.UsageUsec * 1000
			m.CPUUserNanos = stats.CPU.UserUsec * 1000
			m.CPUSystemNanos = stats.CPU.SystemUsec * 1000
			m.ThrottledPeriods = stats.CPU.NrThrottled
			m.ThrottledTimeNanos = stats.CPU.ThrottledUsec * 1000
		}
		if stats.Memory != nil {
			m.MemoryUsageBytes = stats.Memory.Usage
			m.MemoryMaxUsageBytes = stats.Memory.UsageLimit
			m.MemorySwapBytes = stats.Memory.SwapUsage
			m.MemoryRSSBytes = stats.Memory.Anon
			m.MemoryCacheBytes = stats.Memory.File
			m.MemoryMappedBytes = stats.Memory.FileMapped
		}
	default:
		return nil, fmt.Errorf("unsupported metrics type: %T", data)
	}

	return m, nil
}

func (m *containerMetrics) ResourceUsage(previous *containerMetrics) *drivers.TaskResourceUsage {
	memory := &drivers.MemoryStats{
		RSS:        m.MemoryRSSBytes,
		Cache:      m.MemoryCacheBytes,
		Swap:       m.MemorySwapBytes,
		MappedFile: m.MemoryMappedBytes,
		Usage:      m.MemoryUsageBytes,
		MaxUsage:   m.MemoryMaxUsageBytes,
		Measured:   measuredMemoryStats,
	}
	cpu := &drivers.CpuStats{
		ThrottledPeriods: m.ThrottledPeriods,
		ThrottledTime:    m.ThrottledTimeNanos,
		Measured:         measuredCPUStats,
	}

	if previous != nil && m.Timestamp.After(previous.Timestamp) {
		elapsed := float64(m.Timestamp.Sub(previous.Timestamp).Nanoseconds())
		cpu.Percent = percent(previous.CPUUsageNanos, m.CPUUsageNanos, elapsed)
		cpu.UserMode = percent(previous.CPUUserNanos, m.CPUUserNanos, elapsed)
		cpu.SystemMode = percent(previous.CPUSystemNanos, m.CPUSystemNanos, elapsed)
	}

	return &drivers.TaskResourceUsage{
		ResourceUsage: &drivers.ResourceUsage{
			MemoryStats: memory,
			CpuStats:    cpu,
		},
		Timestamp: m.Timestamp.UnixNano(),
	}
}

func percent(previous, current uint64, elapsedNanos float64) float64 {
	if current <= previous || elapsedNanos <= 0 {
		return 0
	}
	return (float64(current-previous) / elapsedNanos) * 100
}
