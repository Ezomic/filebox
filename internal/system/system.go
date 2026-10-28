package system

type Metrics struct {
	CPUPercent    float64  `json:"cpuPercent"`
	MemTotal      uint64   `json:"memTotal"`
	MemUsed       uint64   `json:"memUsed"`
	DiskTotal     uint64   `json:"diskTotal"`
	DiskUsed      uint64   `json:"diskUsed"`
	UptimeSeconds uint64   `json:"uptimeSeconds"`
	TempCelsius   *float64 `json:"tempCelsius"`
}

type Host interface {
	Metrics() (Metrics, error)
}

type Fake struct {
	Value Metrics
	Err   error
}

func (f *Fake) Metrics() (Metrics, error) {
	return f.Value, f.Err
}
