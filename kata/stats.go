package kata

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

func (c *CtrClient) Metrics(ctx context.Context, containerID string) (*containerMetrics, error) {
	out, err := c.run(ctx, "task", "metrics", "--format", "json", containerID)
	if err != nil {
		return nil, err
	}
	return parseContainerMetrics(out, time.Now().UTC())
}

func parseContainerMetrics(raw string, timestamp time.Time) (*containerMetrics, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}

	metrics := &containerMetrics{Timestamp: timestamp}

	if cpu, ok := object(data, "cpu"); ok {
		metrics.CPUUsageNanos = number(cpu, "usage_usec") * 1000
		metrics.CPUUserNanos = number(cpu, "user_usec") * 1000
		metrics.CPUSystemNanos = number(cpu, "system_usec") * 1000
		metrics.ThrottledPeriods = number(cpu, "nr_throttled")
		metrics.ThrottledTimeNanos = number(cpu, "throttled_usec") * 1000

		if usage, ok := object(cpu, "usage"); ok {
			metrics.CPUUsageNanos = number(usage, "total")
			metrics.CPUUserNanos = number(usage, "user")
			metrics.CPUSystemNanos = number(usage, "kernel")
		}
		if throttling, ok := object(cpu, "throttling"); ok {
			metrics.ThrottledPeriods = number(throttling, "throttled_periods")
			metrics.ThrottledTimeNanos = number(throttling, "throttled_time")
		}
	}

	if memory, ok := object(data, "memory"); ok {
		metrics.MemoryUsageBytes = number(memory, "usage")
		metrics.MemoryMaxUsageBytes = number(memory, "usage_limit")
		metrics.MemorySwapBytes = number(memory, "swap_usage")

		if usage, ok := object(memory, "usage"); ok {
			metrics.MemoryUsageBytes = number(usage, "usage")
			metrics.MemoryMaxUsageBytes = number(usage, "max")
			if metrics.MemoryMaxUsageBytes == 0 {
				metrics.MemoryMaxUsageBytes = number(usage, "limit")
			}
		}
		metrics.MemoryRSSBytes = firstNumber(memory, "total_rss", "rss", "anon")
		metrics.MemoryCacheBytes = firstNumber(memory, "total_cache", "cache", "file")
		metrics.MemorySwapBytes = firstNumber(memory, "total_swap", "swap", "swap_usage")
		metrics.MemoryMappedBytes = firstNumber(memory, "total_mapped_file", "mapped_file", "file_mapped")
	}

	return metrics, nil
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

func object(data map[string]any, key string) (map[string]any, bool) {
	value, ok := data[key]
	if !ok {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	return obj, ok
}

func firstNumber(data map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		if value := number(data, key); value != 0 {
			return value
		}
	}
	return 0
}

func number(data map[string]any, key string) uint64 {
	value, ok := data[key]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return uint64(v)
	case json.Number:
		n, _ := v.Int64()
		return uint64(n)
	default:
		return 0
	}
}

func percent(previous, current uint64, elapsedNanos float64) float64 {
	if current <= previous || elapsedNanos <= 0 {
		return 0
	}
	return (float64(current-previous) / elapsedNanos) * 100
}
