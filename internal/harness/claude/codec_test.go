package claude

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
)

var _ = Describe("The sidecar wire's codec", Label("unit"), func() {
	It("renders a request as one line and reads it back unchanged", func() {
		settings, err := structpb.NewStruct(map[string]any{
			"model":  "claude-opus-5",
			"effort": "high",
			"tools":  map[string]any{"allow": []any{"Read"}, "deny": []any{"Write"}},
		})
		Expect(err).NotTo(HaveOccurred())
		request := &harnessv1.Request{
			Id: "mill-0102030405060708",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId:    "b6f1e0a2-0000-4000-8000-000000000001",
				Prompt:       "implement the node\nwith a newline in it",
				Attachments:  []*harnessv1.Attachment{{Name: "skift#3", Source: "predecessor", MediaType: "text/markdown", Content: "done"}},
				Settings:     &harnessv1.Settings{Options: settings},
				Workspace:    &harnessv1.Workspace{Socket: "/run/mill/ws.sock", Token: "step-token-1"},
				OutputSchema: `{"type":"object"}`,
				Cwd:          "/w/checkout",
			}},
		}

		line, err := encodeRequest(request)
		Expect(err).NotTo(HaveOccurred())

		Expect(line).To(HaveSuffix("\n"))
		Expect(line[:len(line)-1]).NotTo(ContainSubstring("\n"), "one message per line, and a newline is the only framing")

		read := &harnessv1.Request{}
		Expect(protojson.Unmarshal(line, read)).To(Succeed())
		Expect(proto.Equal(read, request)).To(BeTrue(), "the request did not survive the wire")
	})

	It("reads a terminal record with structured output and every model it billed", func() {
		output, err := structpb.NewStruct(map[string]any{"decision": "approve", "confidence": 0.9})
		Expect(err).NotTo(HaveOccurred())
		original := &harnessv1.Response{Body: &harnessv1.Response_Terminal{Terminal: &harnessv1.Terminal{
			RequestId:        "mill-0102030405060708",
			FinalMessage:     "reviewed",
			StructuredOutput: output,
			Usage: &harnessv1.Usage{ByModel: map[string]*harnessv1.TokenCounts{
				"claude-opus-5":             {Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789},
				"claude-haiku-4-5-20251001": {Input: 3870, Output: 76},
			}},
			Exit: &harnessv1.ProcessExit{Status: 0},
		}}}
		line, err := protojson.Marshal(original)
		Expect(err).NotTo(HaveOccurred())

		response, err := decodeResponse(line)

		Expect(err).NotTo(HaveOccurred())
		Expect(proto.Equal(response, original)).To(BeTrue())
	})

	It("reads the proto's own field spelling as well as the JSON one", func() {
		protoNames, err := decodeResponse([]byte(`{"terminal":{"request_id":"r","final_message":"m","exit":{"status":-1,"reason":"EXIT_REASON_REFUSED"}}}`))
		Expect(err).NotTo(HaveOccurred())
		jsonNames, err := decodeResponse([]byte(`{"terminal":{"requestId":"r","finalMessage":"m","exit":{"status":-1,"reason":"EXIT_REASON_REFUSED"}}}`))
		Expect(err).NotTo(HaveOccurred())

		Expect(proto.Equal(protoNames, jsonNames)).To(BeTrue())
		Expect(protoNames.GetTerminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
	})

	It("keeps a frame from a newer contract readable", func() {
		response, err := decodeResponse([]byte(`{"terminal":{"final_message":"m","exit":{"status":0}},"a_verb_we_do_not_know":{}}`))

		Expect(err).NotTo(HaveOccurred())
		Expect(response.GetTerminal().GetFinalMessage()).To(Equal("m"))
	})

	It("refuses a line that is not a response at all", func() {
		_, err := decodeResponse([]byte(`not json`))

		Expect(err).To(MatchError(ContainSubstring("decode response")))
	})
})
