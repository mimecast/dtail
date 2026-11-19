package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type sessionGenerationKey struct{}

const generationOutputPrefix = "\x1egen:"

func withSessionGeneration(ctx context.Context, generation uint64) context.Context {
	if ctx == nil {
		panic("handlers: nil session generation context")
	}
	if generation == 0 {
		return ctx
	}
	return context.WithValue(ctx, sessionGenerationKey{}, generation)
}

func sessionGenerationFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		panic("handlers: nil session generation context")
	}

	generation, _ := ctx.Value(sessionGenerationKey{}).(uint64)
	return generation
}

func encodeGeneratedMessage(generation uint64, message string) string {
	if generation == 0 {
		return message
	}
	return fmt.Sprintf("%s%d\x1e%s", generationOutputPrefix, generation, message)
}

func decodeGeneratedMessage(message string) (uint64, string) {
	if !strings.HasPrefix(message, generationOutputPrefix) {
		return 0, message
	}

	rest := strings.TrimPrefix(message, generationOutputPrefix)
	parts := strings.SplitN(rest, "\x1e", 2)
	if len(parts) != 2 {
		return 0, message
	}

	generation, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, message
	}

	return generation, parts[1]
}

func encodeGeneratedBytes(generation uint64, payload []byte) []byte {
	if generation == 0 {
		return payload
	}

	prefix := []byte(fmt.Sprintf("%s%d\x1e", generationOutputPrefix, generation))
	data := make([]byte, 0, len(prefix)+len(payload))
	data = append(data, prefix...)
	data = append(data, payload...)
	return data
}

func decodeGeneratedBytes(payload []byte) (uint64, []byte) {
	message := string(payload)
	generation, decoded := decodeGeneratedMessage(message)
	if generation == 0 {
		return 0, payload
	}
	return generation, []byte(decoded)
}
