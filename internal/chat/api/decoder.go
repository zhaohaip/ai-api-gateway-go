package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

const unknownFieldPrefix = "json: unknown field "

func decodeRequest(reader io.Reader) (openaiapi.ChatCompletionRequest, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	var request openaiapi.ChatCompletionRequest
	if err := decoder.Decode(&request); err != nil {
		return openaiapi.ChatCompletionRequest{}, decodeError(err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return openaiapi.ChatCompletionRequest{}, err
	}
	return request, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return domain.NewInvalidRequestError("request body must contain one JSON object", "", "invalid_json")
}

func decodeError(err error) error {
	message := err.Error()
	if strings.HasPrefix(message, unknownFieldPrefix) {
		field := strings.Trim(strings.TrimPrefix(message, unknownFieldPrefix), "\"")
		return domain.NewInvalidRequestError(
			fmt.Sprintf("parameter %q is not supported", field),
			field,
			"unsupported_parameter",
		)
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		param := typeErr.Field
		return domain.NewInvalidRequestError(
			fmt.Sprintf("parameter %q has an invalid type", param),
			param,
			"invalid_type",
		)
	}
	if errors.Is(err, io.EOF) {
		return domain.NewInvalidRequestError("request body is required", "", "invalid_json")
	}
	return domain.NewInvalidRequestError("request body is not valid JSON", "", "invalid_json")
}
