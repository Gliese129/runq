package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
)

type activityStubBackend struct {
	*backend.UnavailableBackend
	jobID string
	resp  *backend.JobActivity
}

func (b *activityStubBackend) JobActivity(_ context.Context, jobID string) (*backend.JobActivity, error) {
	b.jobID = jobID
	return b.resp, nil
}

// The activity handler relays the backend's decimated series verbatim:
// legacy rows keep lines:null on the wire, live jobs omit job_end.
func TestHandleJobActivity(t *testing.T) {
	lines := int64(120)
	be := &activityStubBackend{
		UnavailableBackend: backend.NewUnavailableBackend(errors.New("unused")),
		resp: &backend.JobActivity{
			JobStart: 1000,
			Tasks: []backend.TaskActivity{
				{TaskID: "t1", Status: "running", BucketMin: 5, Points: []backend.ActivityPoint{
					{TS: 1060, Bytes: 4096, Lines: &lines},
				}},
				{TaskID: "t2", Status: "pending", BucketMin: 1, Points: []backend.ActivityPoint{
					{TS: 1120, Bytes: 512}, // legacy 2-column: lines null
				}},
			},
		},
	}
	srv := NewServer(be, &config.GlobalConfig{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs/j1/activity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if be.jobID != "j1" {
		t.Fatalf("backend asked for job %q", be.jobID)
	}

	var got struct {
		JobStart int64  `json:"job_start"`
		JobEnd   *int64 `json:"job_end"`
		Tasks    []struct {
			TaskID    string `json:"task_id"`
			BucketMin int    `json:"bucket_minutes"`
			Points    []struct {
				TS    int64  `json:"ts"`
				Bytes int64  `json:"bytes"`
				Lines *int64 `json:"lines"`
			} `json:"points"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got.JobStart != 1000 || got.JobEnd != nil {
		t.Fatalf("window: start=%d end=%v", got.JobStart, got.JobEnd)
	}
	if len(got.Tasks) != 2 || got.Tasks[0].BucketMin != 5 {
		t.Fatalf("tasks shape wrong: %+v", got.Tasks)
	}
	if got.Tasks[0].Points[0].Lines == nil || *got.Tasks[0].Points[0].Lines != 120 {
		t.Fatal("3-column lines lost")
	}
	if got.Tasks[1].Points[0].Lines != nil {
		t.Fatal("legacy row must serialize lines as null")
	}
}
