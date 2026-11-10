package qos

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ScheduleRule defines a speed limit active from a specific time of day (HH:MM)
type ScheduleRule struct {
	MinuteOfDay int   // 0 .. 1439
	RateBytes   int64 // Bytes per second (0 = unlimited)
}

// BandwidthThrottler manages dynamic rate limiting according to a 24h timetable
type BandwidthThrottler struct {
	mu      sync.RWMutex
	rules   []ScheduleRule
	enabled bool
}

// ParseBandwidthSchedule parses string like "07:00,5M 23:00,18M" or "10M"
func ParseBandwidthSchedule(configStr string) (*BandwidthThrottler, error) {
	bt := &BandwidthThrottler{enabled: true}
	configStr = strings.TrimSpace(configStr)
	if configStr == "" || configStr == "0" || strings.EqualFold(configStr, "unlimited") {
		bt.enabled = false
		return bt, nil
	}

	parts := strings.Fields(configStr)
	for _, part := range parts {
		if strings.Contains(part, ",") {
			// Schedule format: "07:00,5M"
			tokens := strings.SplitN(part, ",", 2)
			minOfDay, err := parseTimeOfDay(tokens[0])
			if err != nil {
				return nil, fmt.Errorf("invalid time format in %q: %w", part, err)
			}
			rate, err := parseRate(tokens[1])
			if err != nil {
				return nil, fmt.Errorf("invalid rate format in %q: %w", part, err)
			}
			bt.rules = append(bt.rules, ScheduleRule{MinuteOfDay: minOfDay, RateBytes: rate})
		} else {
			// Static format: "10M"
			rate, err := parseRate(part)
			if err != nil {
				return nil, fmt.Errorf("invalid rate format %q: %w", part, err)
			}
			bt.rules = append(bt.rules, ScheduleRule{MinuteOfDay: 0, RateBytes: rate})
		}
	}

	sort.Slice(bt.rules, func(i, j int) bool {
		return bt.rules[i].MinuteOfDay < bt.rules[j].MinuteOfDay
	})

	return bt, nil
}

func parseTimeOfDay(tStr string) (int, error) {
	parts := strings.Split(tStr, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM, got %s", tStr)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("invalid hour %s", parts[0])
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid minute %s", parts[1])
	}
	return h*60 + m, nil
}

func parseRate(rateStr string) (int64, error) {
	rateStr = strings.TrimSpace(strings.ToUpper(rateStr))
	if rateStr == "" || rateStr == "0" || rateStr == "UNLIMITED" {
		return 0, nil
	}

	multiplier := int64(1)
	if strings.HasSuffix(rateStr, "K") || strings.HasSuffix(rateStr, "KB") {
		multiplier = 1024
		rateStr = strings.TrimSuffix(strings.TrimSuffix(rateStr, "B"), "K")
	} else if strings.HasSuffix(rateStr, "M") || strings.HasSuffix(rateStr, "MB") {
		multiplier = 1024 * 1024
		rateStr = strings.TrimSuffix(strings.TrimSuffix(rateStr, "B"), "M")
	} else if strings.HasSuffix(rateStr, "G") || strings.HasSuffix(rateStr, "GB") {
		multiplier = 1024 * 1024 * 1024
		rateStr = strings.TrimSuffix(strings.TrimSuffix(rateStr, "B"), "G")
	}

	val, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return 0, err
	}
	return int64(val * float64(multiplier)), nil
}

// CurrentLimitBytes returns current limit for this minute of the day
func (bt *BandwidthThrottler) CurrentLimitBytes() int64 {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	if !bt.enabled || len(bt.rules) == 0 {
		return 0
	}

	now := time.Now()
	currentMin := now.Hour()*60 + now.Minute()

	activeRate := bt.rules[len(bt.rules)-1].RateBytes // Default to last rule if before first
	for _, rule := range bt.rules {
		if currentMin >= rule.MinuteOfDay {
			activeRate = rule.RateBytes
		} else {
			break
		}
	}

	return activeRate
}

// ThrottledReader wraps an io.Reader and limits read speed according to the schedule
type ThrottledReader struct {
	r         io.Reader
	throttler *BandwidthThrottler
	lastRead  time.Time
}

// NewThrottledReader creates a rate-limited reader
func NewThrottledReader(r io.Reader, throttler *BandwidthThrottler) *ThrottledReader {
	return &ThrottledReader{
		r:         r,
		throttler: throttler,
		lastRead:  time.Now(),
	}
}

func (tr *ThrottledReader) Read(p []byte) (int, error) {
	limit := tr.throttler.CurrentLimitBytes()
	if limit <= 0 {
		return tr.r.Read(p)
	}

	// Limit buffer size to 64KB chunks to keep throttling smooth
	maxChunk := int(limit / 10)
	if maxChunk < 4096 {
		maxChunk = 4096
	}
	if maxChunk > 65536 {
		maxChunk = 65536
	}

	buf := p
	if len(buf) > maxChunk {
		buf = buf[:maxChunk]
	}

	n, err := tr.r.Read(buf)
	if n > 0 {
		// Calculate expected time for n bytes
		expectedDuration := time.Duration(float64(n) / float64(limit) * float64(time.Second))
		elapsed := time.Since(tr.lastRead)
		if elapsed < expectedDuration {
			time.Sleep(expectedDuration - elapsed)
		}
		tr.lastRead = time.Now()
	}

	return n, err
}
