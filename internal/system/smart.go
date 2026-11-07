package system

import (
	"encoding/json"
	"errors"
)

var ErrSmartUnavailable = errors.New("disk health unavailable")

type DiskHealth struct {
	Device      string   `json:"device"`
	Model       string   `json:"model"`
	Healthy     *bool    `json:"healthy"`
	TempCelsius *float64 `json:"tempCelsius"`
	// Serial and Reallocated are for the alerts. Reallocated is the reallocated sector count (ATA attribute 5),
	// nil when the disk does not report one, such as an NVMe disk or one asleep.
	Serial      string  `json:"-"`
	Reallocated *uint64 `json:"-"`
}

const reallocatedSectors = 5

type Smart interface {
	Disks() ([]DiskHealth, error)
}

type smartctlReport struct {
	Device struct {
		Name string `json:"name"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	SmartStatus  *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	Attributes *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

// parseSmartReport reads one smartctl JSON document; ok is false when it is not one.
func parseSmartReport(line []byte) (DiskHealth, bool) {
	var report smartctlReport
	if json.Unmarshal(line, &report) != nil || report.Device.Name == "" {
		return DiskHealth{}, false
	}
	disk := DiskHealth{Device: report.Device.Name, Model: report.ModelName, Serial: report.SerialNumber, Reallocated: report.reallocated()}
	if report.SmartStatus != nil {
		passed := report.SmartStatus.Passed
		disk.Healthy = &passed
	}
	if report.Temperature != nil {
		celsius := report.Temperature.Current
		disk.TempCelsius = &celsius
	}

	return disk, true
}

func (r smartctlReport) reallocated() *uint64 {
	if r.Attributes == nil {
		return nil
	}
	for _, attribute := range r.Attributes.Table {
		if attribute.ID == reallocatedSectors && attribute.Raw.Value >= 0 {
			count := uint64(attribute.Raw.Value)
			return &count
		}
	}

	return nil
}

type FakeSmart struct {
	Value []DiskHealth
	Err   error
}

func (f *FakeSmart) Disks() ([]DiskHealth, error) {
	return f.Value, f.Err
}

func NewFakeSmart() *FakeSmart {
	healthy, failing := true, false
	warm, hot := 34.0, 51.0

	return &FakeSmart{Value: []DiskHealth{
		{Device: "/dev/sda", Model: "WDC WD5000AAKX", Healthy: &healthy, TempCelsius: &warm, Serial: "WD-WCAYUJ123456"},
		{Device: "/dev/sdb", Model: "ST1000DM003", Healthy: &failing, TempCelsius: &hot, Serial: "Z1D2ABCD"},
	}}
}
