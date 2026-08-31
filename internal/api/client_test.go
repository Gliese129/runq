package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestClientPublishesRunqdProtocolVersion(t *testing.T) {
	client := &Client{httpc: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("X-Runqd-Protocol"); got != MachineProtocolVersion {
			t.Fatalf("machine protocol header = %q, want %q", got, MachineProtocolVersion)
		}
		return response(`{"ready":true}`), nil
	})}}
	resp, err := client.Do(context.Background(), http.MethodGet, "/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestMachineGPUStatusDecodesStandaloneRunqdShape(t *testing.T) {
	client := &Client{httpc: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`[{"index":1,"name":"GPU","mem_total":1000,"mem_free":250,"util_pct":80,"attempt_id":"attempt-1","task_id":"attempt-1"}]`), nil
	})}}
	gpus, err := NewProxyFromClient(client).MachineGPUStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 || gpus[0].MemTotalMB != 1000 || gpus[0].MemUsedMB != 750 ||
		gpus[0].UtilPercent != 80 || gpus[0].TaskID != "attempt-1" {
		t.Fatalf("machine GPU projection = %+v", gpus)
	}
}
