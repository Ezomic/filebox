package system

import (
	"errors"
	"testing"
	"time"
)

const sampleSmart = `{"device":{"name":"/dev/sda"},"model_name":"WDC WD5000AAKX","smart_status":{"passed":true},"temperature":{"current":34}}
not json at all
{"device":{"name":"/dev/sdb"},"model_name":"ST1000DM003","smart_status":{"passed":false}}
{"device":{"name":"/dev/sdc"}}
{"model_name":"no device name"}
`

func TestParseSmartLines(t *testing.T) {
	disks := parseSmartLines(sampleSmart)
	if len(disks) != 3 {
		t.Fatalf("got %d disks: %+v", len(disks), disks)
	}
	if disks[0].Healthy == nil || !*disks[0].Healthy || disks[0].TempCelsius == nil || *disks[0].TempCelsius != 34 {
		t.Fatalf("sda: %+v", disks[0])
	}
	if disks[1].Healthy == nil || *disks[1].Healthy || disks[1].TempCelsius != nil {
		t.Fatalf("sdb: %+v", disks[1])
	}
	if disks[2].Healthy != nil || disks[2].TempCelsius != nil {
		t.Fatalf("sdc must be unknown, got %+v", disks[2])
	}
}

func TestParseSmartLinesEmptyIsNotNil(t *testing.T) {
	if disks := parseSmartLines(""); disks == nil || len(disks) != 0 {
		t.Fatalf("got %#v", disks)
	}
}

func TestCachedSmartReadsOncePerMinute(t *testing.T) {
	now := time.Now()
	reads := 0
	c := &cachedSmart{now: func() time.Time { return now }, read: func() ([]DiskHealth, error) {
		reads++
		return []DiskHealth{{Device: "/dev/sda"}}, nil
	}}

	for range 3 {
		if _, err := c.Disks(); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("reads=%d", reads)
	}
	now = now.Add(smartTTL + time.Second)
	_, _ = c.Disks()
	if reads != 2 {
		t.Fatalf("reads=%d after expiry", reads)
	}
}

func TestCachedSmartDoesNotCacheFailures(t *testing.T) {
	reads := 0
	c := &cachedSmart{now: time.Now, read: func() ([]DiskHealth, error) {
		reads++
		return nil, ErrSmartUnavailable
	}}
	for range 2 {
		if _, err := c.Disks(); !errors.Is(err, ErrSmartUnavailable) {
			t.Fatalf("got %v", err)
		}
	}
	if reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
}

func TestReadSmartHelperReportsMissingCommand(t *testing.T) {
	if _, err := readSmartHelper([]string{"this-command-does-not-exist"}); !errors.Is(err, ErrSmartUnavailable) {
		t.Fatalf("got %v", err)
	}
}
