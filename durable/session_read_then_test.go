package durable_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Cases beyond pi: ReadOnLineThen's continuation gets its first Storage call
// even once Close has begun, as pi's continuation issues it ahead of the close
// on the line; later calls are refused, as in pi.

func TestReadOnLineThenAdmitsTheContinuationsFirstStorageCallOnceCloseBegan(t *testing.T) {
	ts := openTestSession(t)
	id := createConversation(t, ts.session)
	entered, release := make(chan struct{}), make(chan struct{})
	var first, second error
	read := make(chan error, 1)
	go func() {
		read <- ts.session.ReadOnLineThen(bg, func() error {
			_, err := ts.session.Storage().Conversation(bg, id)
			return err
		}, func(ctx context.Context) error {
			close(entered)
			<-release
			_, first = ts.session.Storage().Conversation(ctx, id)
			_, second = ts.session.Storage().Conversation(ctx, id)
			return nil
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- ts.session.Close(bg) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) before the reserved call ran", err)
	case <-time.After(50 * time.Millisecond):
	}
	// The seal still refuses new work meanwhile.
	if err := ts.session.ReadOnLine(func() error { return nil }); err == nil {
		t.Fatal("a read after the seal was admitted")
	}
	close(release)
	if err := <-read; err != nil {
		t.Fatalf("read = %v", err)
	}
	if first != nil {
		t.Fatalf("first call = %v", first)
	}
	if second == nil || !strings.Contains(second.Error(), "is closed") {
		t.Fatalf("second call = %v, want refused as closed", second)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if ts.storage.checkOpen() == nil {
		t.Fatal("Storage is still open")
	}
}

func TestReadOnLineThenHoldsCloseOnlyUntilAContinuationWithoutStorageCallsEnds(t *testing.T) {
	ts := openTestSession(t)
	entered, release := make(chan struct{}), make(chan struct{})
	read := make(chan error, 1)
	go func() {
		read <- ts.session.ReadOnLineThen(bg, func() error { return nil }, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- ts.session.Close(bg) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the reservation was held", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close still waits after the continuation ended")
	}
}

func TestReadOnLineThenRunsNothingOffLineWhenTheLineReadFails(t *testing.T) {
	ts := openTestSession(t)
	failure := errors.New("read failed")
	ran := false
	err := ts.session.ReadOnLineThen(bg, func() error { return failure }, func(context.Context) error {
		ran = true
		return nil
	})
	if !errors.Is(err, failure) || ran {
		t.Fatalf("err = %v, then ran %v", err, ran)
	}
	// No reservation is left behind: Close does not wait.
	done := make(chan error, 1)
	go func() { done <- ts.session.Close(bg) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waits for a read that never went off the line")
	}
}
