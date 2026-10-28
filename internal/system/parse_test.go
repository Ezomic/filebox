package system

import "testing"

func TestParseCPUStat(t *testing.T) {
	sample, err := parseCPUStat("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4 5\n")
	if err != nil {
		t.Fatal(err)
	}
	if sample.total != 1000 || sample.idle != 850 {
		t.Fatalf("got %+v", sample)
	}
}

func TestParseCPUStatRejectsGarbage(t *testing.T) {
	if _, err := parseCPUStat("nonsense"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCPUPercent(t *testing.T) {
	got := cpuPercent(cpuSample{idle: 800, total: 1000}, cpuSample{idle: 850, total: 1100})
	if got != 50 {
		t.Fatalf("got %v", got)
	}
}

func TestCPUPercentHandlesNoProgress(t *testing.T) {
	same := cpuSample{idle: 10, total: 20}
	if got := cpuPercent(same, same); got != 0 {
		t.Fatalf("got %v", got)
	}
	if got := cpuPercent(cpuSample{total: 50}, cpuSample{total: 40}); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, used, err := parseMeminfo("MemTotal:  4000000 kB\nMemFree: 100 kB\nMemAvailable: 1000000 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4000000*1024 || used != 3000000*1024 {
		t.Fatalf("got %d %d", total, used)
	}
}

func TestParseMeminfoRejectsMissingFields(t *testing.T) {
	if _, _, err := parseMeminfo("MemTotal: 10 kB\n"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseUptime(t *testing.T) {
	got, err := parseUptime("93784.55 180000.10\n")
	if err != nil || got != 93784 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := parseUptime("x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseMillidegrees(t *testing.T) {
	got, err := parseMillidegrees("45500\n")
	if err != nil || got != 45.5 {
		t.Fatalf("got %v, %v", got, err)
	}
}
