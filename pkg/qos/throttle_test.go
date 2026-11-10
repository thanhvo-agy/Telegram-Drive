package qos

import (
	"testing"
)

func TestParseBandwidthSchedule(t *testing.T) {
	schedule := "07:00,5M 23:00,18M"
	bt, err := ParseBandwidthSchedule(schedule)
	if err != nil {
		t.Fatalf("ParseBandwidthSchedule failed: %v", err)
	}

	if len(bt.rules) != 2 {
		t.Fatalf("Expected 2 rules, got %d", len(bt.rules))
	}

	if bt.rules[0].MinuteOfDay != 7*60 {
		t.Errorf("Expected 420 mins, got %d", bt.rules[0].MinuteOfDay)
	}
	if bt.rules[0].RateBytes != 5*1024*1024 {
		t.Errorf("Expected 5MB/s (%d), got %d", 5*1024*1024, bt.rules[0].RateBytes)
	}

	if bt.rules[1].MinuteOfDay != 23*60 {
		t.Errorf("Expected 1380 mins, got %d", bt.rules[1].MinuteOfDay)
	}
	if bt.rules[1].RateBytes != 18*1024*1024 {
		t.Errorf("Expected 18MB/s (%d), got %d", 18*1024*1024, bt.rules[1].RateBytes)
	}

	limit := bt.CurrentLimitBytes()
	if limit <= 0 {
		t.Errorf("Expected active rate > 0, got %d", limit)
	}
}
