package api

import (
	"sync"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
	"github.com/zhaohaip/ai-api-gateway-go/internal/concurrencylimit"
)

// concurrencyChatStream 将并发槽位的生命周期绑定到内部流的终止和关闭。
type concurrencyChatStream struct {
	stream    domain.ChatStream
	lease     concurrencylimit.Lease
	closeOnce sync.Once
	closeErr  error
}

func newConcurrencyChatStream(
	stream domain.ChatStream,
	lease concurrencylimit.Lease,
) *concurrencyChatStream {
	return &concurrencyChatStream{stream: stream, lease: lease}
}

func (s *concurrencyChatStream) Recv() (domain.ChatChunk, error) {
	chunk, err := s.stream.Recv()
	if err != nil {
		s.lease.Release()
	}
	return chunk, err
}

func (s *concurrencyChatStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.stream.Close()
		s.lease.Release()
	})
	return s.closeErr
}
