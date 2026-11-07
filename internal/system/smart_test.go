package system

import (
	"strings"
	"testing"
)

const sampleSmart = `{"device":{"name":"/dev/sda"},"model_name":"WDC WD5000AAKX","smart_status":{"passed":true},"temperature":{"current":34}}
not json at all
{"device":{"name":"/dev/sdb"},"model_name":"ST1000DM003","smart_status":{"passed":false}}
{"device":{"name":"/dev/sdc"}}
{"model_name":"no device name"}
`

// parseSmartLines reads one smartctl JSON document per line and skips the lines that are not one.
func parseSmartLines(output string) []DiskHealth {
	var disks []DiskHealth
	for _, line := range strings.Split(output, "\n") {
		if disk, ok := parseSmartReport([]byte(line)); ok {
			disks = append(disks, disk)
		}
	}

	return disks
}

func TestParseSmartReports(t *testing.T) {
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

func TestParseSmartReportReadsTheSerialAndReallocatedSectors(t *testing.T) {
	line := `{"device":{"name":"/dev/sda"},"serial_number":"WD-WCAYUJ123456","smart_status":{"passed":true},` +
		`"ata_smart_attributes":{"table":[{"id":1,"raw":{"value":7}},{"id":5,"raw":{"value":12}},{"id":197,"raw":{"value":3}}]}}
{"device":{"name":"/dev/nvme0"},"serial_number":"S4EVNF0M","smart_status":{"passed":true},"nvme_smart_health_information_log":{"media_errors":0}}
{"device":{"name":"/dev/sdb"},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":-1}}]}}
`
	disks := parseSmartLines(line)
	if len(disks) != 3 {
		t.Fatalf("got %+v", disks)
	}
	if disks[0].Serial != "WD-WCAYUJ123456" || disks[0].Reallocated == nil || *disks[0].Reallocated != 12 {
		t.Fatalf("sda: %+v", disks[0])
	}
	if disks[1].Serial != "S4EVNF0M" || disks[1].Reallocated != nil {
		t.Fatalf("an NVMe disk has no reallocated sector count: %+v", disks[1])
	}
	if disks[2].Reallocated != nil {
		t.Fatalf("a nonsense count is no count: %+v", disks[2])
	}
}
