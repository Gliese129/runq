package store

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskAttemptLockSerializesSameTask(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	secondStarted := make(chan struct{})
	done := make(chan struct{})
	var secondEntered atomic.Bool
	go func() {
		_ = st.WithTaskAttemptLock("task", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() {
		close(secondStarted)
		_ = st.WithTaskAttemptLock("task", func() error {
			secondEntered.Store(true)
			return nil
		})
		close(done)
	}()
	<-secondStarted
	time.Sleep(20 * time.Millisecond)
	if secondEntered.Load() {
		t.Fatal("same-task attempt effects overlapped")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second same-task attempt effect did not resume")
	}
}
