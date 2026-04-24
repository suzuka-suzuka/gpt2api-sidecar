package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGateHandsSlotToNextWaiter(t *testing.T) {
	g := New(1, 4)

	first, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer first.Release()

	acquired := make(chan *Lease, 1)
	go func() {
		lease, err := g.Acquire(context.Background())
		if err != nil {
			t.Errorf("acquire second: %v", err)
			return
		}
		acquired <- lease
	}()

	select {
	case lease := <-acquired:
		lease.Release()
		t.Fatal("second waiter acquired before first release")
	case <-time.After(100 * time.Millisecond):
	}

	first.Release()

	select {
	case lease := <-acquired:
		lease.Release()
	case <-time.After(1 * time.Second):
		t.Fatal("second waiter did not acquire after release")
	}
}

func TestGateRejectsWhenQueueIsFull(t *testing.T) {
	g := New(1, 1)

	first, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer first.Release()

	waitingCtx, cancelWaiting := context.WithCancel(context.Background())
	defer cancelWaiting()
	waitingDone := make(chan struct{})
	go func() {
		defer close(waitingDone)
		lease, err := g.Acquire(waitingCtx)
		if err == nil {
			lease.Release()
		}
	}()

	time.Sleep(50 * time.Millisecond)

	_, err = g.Acquire(context.Background())
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
}

func TestGateSkipsCanceledWaiter(t *testing.T) {
	g := New(1, 4)

	first, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer first.Release()

	cancelCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cancelDone := make(chan error, 1)
	go func() {
		_, err := g.Acquire(cancelCtx)
		cancelDone <- err
	}()

	time.Sleep(100 * time.Millisecond)

	acquired := make(chan *Lease, 1)
	go func() {
		lease, err := g.Acquire(context.Background())
		if err != nil {
			t.Errorf("acquire third: %v", err)
			return
		}
		acquired <- lease
	}()

	first.Release()

	select {
	case err := <-cancelDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected canceled waiter to time out, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("canceled waiter did not finish")
	}

	select {
	case lease := <-acquired:
		lease.Release()
	case <-time.After(1 * time.Second):
		t.Fatal("next waiter did not acquire after canceled waiter")
	}
}
