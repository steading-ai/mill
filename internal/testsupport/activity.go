package testsupport

import (
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// AppendActivity writes one or more activity records to path, length-prefixed
// exactly as the writer does (internal/run/activity.go), so a spec can hand a
// fixture attempt to `mill logs` or the `Logs` RPC without driving a real one
// through the worker.
func AppendActivity(path string, activities ...*millv1.Activity) {
	ginkgo.GinkgoHelper()
	gomega.Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(gomega.Succeed())
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { gomega.Expect(file.Close()).To(gomega.Succeed()) }()
	for _, a := range activities {
		raw, err := proto.Marshal(a)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
		_, err = file.Write(header[:])
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = file.Write(raw)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

// TextActivity is a harness `text` record, the simplest kind that renders as
// one line: "text <content>".
func TextActivity(text string) *millv1.Activity {
	return &millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
		Body:   &millv1.Activity_Text{Text: &millv1.Text{Text: text, Final: true}},
	}
}
