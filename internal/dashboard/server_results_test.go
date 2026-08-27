package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

type resultsStubBackend struct {
	*backend.UnavailableBackend
	jobID string
	resp  *backend.JobResults
}

func (b *resultsStubBackend) JobResults(_ context.Context, jobID string) (*backend.JobResults, error) {
	b.jobID = jobID
	return b.resp, nil
}

// The results handler relays the backend's columnar wire verbatim: nil
// metric holes serialize as JSON null, ranges and vocab pass through.
func TestHandleJobResults(t *testing.T) {
	v := 24.8
	be := &resultsStubBackend{
		UnavailableBackend: backend.NewUnavailableBackend(errors.New("unused")),
		resp: &backend.JobResults{
			Source: backend.ResultSource, Parsed: 2, Skipped: 3, Truncated: true,
			UpdatedAt: 200, N: 2,
			Schema: backend.ResultSchema{
				Groups:  []backend.ResultRange{{Key: "a", Offset: 0, Count: 2}},
				Tasks:   []backend.ResultRange{{ID: "t1", Offset: 0, Count: 2}},
				Axes:    map[string]backend.ResultAxis{"model": {Type: "str", Role: "identity", Vocab: []string{"a"}}},
				XAxes:   []string{},
				Metrics: []string{"math"},
			},
			Cols: backend.ResultCols{
				TS:      []int64{100, 200},
				Axes:    map[string][]any{"model": {0, 0}},
				Metrics: map[string][]*float64{"math": {&v, nil}},
			},
		},
	}
	srv := NewServer(be, &config.GlobalConfig{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs/j1/results", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if be.jobID != "j1" {
		t.Fatalf("backend asked for job %q", be.jobID)
	}

	var got struct {
		Source    string `json:"source"`
		Skipped   int64  `json:"skipped"`
		Truncated bool   `json:"truncated"`
		Schema    struct {
			Groups []struct {
				Key    string `json:"key"`
				Offset int    `json:"offset"`
				Count  int    `json:"count"`
			} `json:"groups"`
			XAxes []string `json:"x_axes"`
		} `json:"schema"`
		Cols struct {
			Metrics map[string][]*float64 `json:"metrics"`
		} `json:"cols"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != backend.ResultSource || got.Skipped != 3 || !got.Truncated {
		t.Errorf("header fields: %+v", got)
	}
	if len(got.Schema.Groups) != 1 || got.Schema.Groups[0].Key != "a" || got.Schema.Groups[0].Count != 2 {
		t.Errorf("groups: %+v", got.Schema.Groups)
	}
	if got.Schema.XAxes == nil {
		t.Errorf("x_axes must serialize as [] not null")
	}
	m := got.Cols.Metrics["math"]
	if len(m) != 2 || m[0] == nil || *m[0] != 24.8 || m[1] != nil {
		t.Errorf("metric column null hole mangled: %v", m)
	}
}
