package api

import "context"

// MachineProtocolVersion is the independently versioned runq-lab/runqd
// contract. Product versions may move independently while this value remains
// compatible.
const MachineProtocolVersion = "1"

// MachineCapabilities is returned before a client relies on optional daemon
// behavior. MinClientProtocol is inclusive.
type MachineCapabilities struct {
	ProtocolVersion   string   `json:"protocol_version"`
	MinClientProtocol string   `json:"min_client_protocol"`
	Features          []string `json:"features"`
}

// MachineHealth distinguishes a ready execution service from a process that
// merely owns a socket.
type MachineHealth struct {
	Ready           bool   `json:"ready"`
	ProtocolVersion string `json:"protocol_version"`
	Running         int    `json:"running"`
	Pending         int    `json:"pending"`
	GPUsFree        int    `json:"gpus_free"`
}

func (p *Proxy) MachineCapabilities(ctx context.Context) (*MachineCapabilities, error) {
	var out MachineCapabilities
	if err := p.do(ctx, "GET", "/api/v1/capabilities", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *Proxy) MachineHealth(ctx context.Context) (*MachineHealth, error) {
	var out MachineHealth
	if err := p.do(ctx, "GET", "/api/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
