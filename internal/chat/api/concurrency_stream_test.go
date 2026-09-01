package api

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

type countingLease struct {
	once  sync.Once
	mu    sync.Mutex
	count int
}

func (l *countingLease) Release() {
	l.once.Do(func() {
		l.mu.Lock()
		l.count++
		l.mu.Unlock()
	})
}

func (l *countingLease) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

type concurrencyStreamStub struct {
	mu         sync.Mutex
	recvErr    error
	closeCount int
	closeErr   error
}

func (s *concurrencyStreamStub) Recv() (domain.ChatChunk, error) {
	return domain.ChatChunk{}, s.recvErr
}

func (s *concurrencyStreamStub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCount++
	return s.closeErr
}

func (s *concurrencyStreamStub) CloseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func TestConcurrencyChatStreamReleasesOnReceiveTermination(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "EOF", err: io.EOF},
		{name: "read error", err: errors.New("upstream read failed")},
		{name: "context canceled", err: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &countingLease{}
			underlying := &concurrencyStreamStub{recvErr: test.err}
			stream := newConcurrencyChatStream(underlying, lease)

			if _, err := stream.Recv(); !errors.Is(err, test.err) {
				t.Fatalf("Recv() error = %v, want %v", err, test.err)
			}
			if lease.Count() != 1 {
				t.Fatalf("release count after Recv = %d, want 1", lease.Count())
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("second Close() error = %v", err)
			}
			if underlying.CloseCount() != 1 || lease.Count() != 1 {
				t.Fatalf("close count = %d, release count = %d", underlying.CloseCount(), lease.Count())
			}
		})
	}
}

func TestConcurrencyChatStreamKeepsLeaseUntilCloseAfterSuccessfulRead(t *testing.T) {
	lease := &countingLease{}
	underlying := &concurrencyStreamStub{}
	stream := newConcurrencyChatStream(underlying, lease)

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if lease.Count() != 0 {
		t.Fatalf("release count after successful read = %d, want 0", lease.Count())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if lease.Count() != 1 || underlying.CloseCount() != 1 {
		t.Fatalf("release count = %d, close count = %d", lease.Count(), underlying.CloseCount())
	}
}

func TestConcurrencyChatStreamConcurrentCloseOnlyClosesAndReleasesOnce(t *testing.T) {
	lease := &countingLease{}
	underlying := &concurrencyStreamStub{}
	stream := newConcurrencyChatStream(underlying, lease)
	var waitGroup sync.WaitGroup
	for index := 0; index < 100; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if err := stream.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}()
	}
	waitGroup.Wait()
	if lease.Count() != 1 || underlying.CloseCount() != 1 {
		t.Fatalf("release count = %d, close count = %d", lease.Count(), underlying.CloseCount())
	}
}

func TestConcurrencyChatStreamReleasesWhenUnderlyingCloseFails(t *testing.T) {
	closeErr := errors.New("close failed")
	lease := &countingLease{}
	underlying := &concurrencyStreamStub{closeErr: closeErr}
	stream := newConcurrencyChatStream(underlying, lease)

	if err := stream.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, closeErr)
	}
	if err := stream.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("second Close() error = %v, want %v", err, closeErr)
	}
	if lease.Count() != 1 || underlying.CloseCount() != 1 {
		t.Fatalf("release count = %d, close count = %d", lease.Count(), underlying.CloseCount())
	}
}
