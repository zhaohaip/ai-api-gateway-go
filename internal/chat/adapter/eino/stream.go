package eino

import (
	"errors"
	"io"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

type streamReader interface {
	Recv() (*schema.Message, error)
	Close()
}

type chatStream struct {
	reader    streamReader
	closeOnce sync.Once
}

func newChatStream(reader streamReader) *chatStream {
	return &chatStream{reader: reader}
}

func (s *chatStream) Recv() (domain.ChatChunk, error) {
	message, err := s.reader.Recv()
	if errors.Is(err, io.EOF) {
		return domain.ChatChunk{}, io.EOF
	}
	if err != nil {
		return domain.ChatChunk{}, classifyProviderError(err)
	}
	chunk, err := toDomainChunk(message)
	if err != nil {
		return domain.ChatChunk{}, upstreamServiceError(err)
	}
	return chunk, nil
}

func (s *chatStream) Close() error {
	s.closeOnce.Do(s.reader.Close)
	return nil
}
