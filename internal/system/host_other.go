//go:build !linux

package system

func NewHost(_ string) Host {
	temp := 41.0

	return &Fake{Value: Metrics{
		CPUPercent:    23.5,
		MemTotal:      4 << 30,
		MemUsed:       1 << 30,
		DiskTotal:     500 << 30,
		DiskUsed:      120 << 30,
		UptimeSeconds: 93784,
		TempCelsius:   &temp,
	}}
}
