package claudecode

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

// The wire is newline-delimited protojson: one harness.v1 message per line,
// no framing beyond the newline. Both ends are generated from
// proto/harness/v1/harness.proto, so neither hand-writes a frame.

// marshal renders a request as the one line the sidecar reads. Field names
// are the proto's own spelling, which the sidecar's parser and Go's both
// accept, so a frame read by eye matches the contract it was generated from.
var marshal = protojson.MarshalOptions{UseProtoNames: true}

// unmarshal is deliberately not strict about unknown fields: a sidecar built
// from a newer contract must not make an older worker drop a terminal record
// it could otherwise read.
var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// encodeRequest renders a request as one line, newline included.
func encodeRequest(request *harnessv1.Request) ([]byte, error) {
	line, err := marshal.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("claudecode: encode request: %w", err)
	}
	return append(line, '\n'), nil
}

// decodeResponse parses one line the sidecar wrote.
func decodeResponse(line []byte) (*harnessv1.Response, error) {
	response := &harnessv1.Response{}
	if err := unmarshal.Unmarshal(line, response); err != nil {
		return nil, fmt.Errorf("claudecode: decode response: %w", err)
	}
	return response, nil
}
