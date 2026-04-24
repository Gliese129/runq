package resource

// Detector abstracts GPU discovery for testability.
type Detector interface {
	Detect() ([]Info, error)
}

// NvidiaSMIDetector discovers GPUs via nvidia-smi.
type NvidiaSMIDetector struct{}

func (NvidiaSMIDetector) Detect() ([]Info, error) {
	return Detect()
}

// MockDetector returns a fixed GPU list (for tests without GPU hardware).
type MockDetector struct {
	GPUs []Info
}

func (m MockDetector) Detect() ([]Info, error) {
	return m.GPUs, nil
}
