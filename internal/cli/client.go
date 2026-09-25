package cli

import (
	"context"
	"net"
	"net/http"

	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
)

// socketURL is the base every request is made against. The host is never
// resolved — the transport below dials the unix socket whatever it says —
// but Connect requires a well-formed URL to build a path from.
const socketURL = "http://mill"

// storeClient talks to `mill serve` over its unix socket. The caller is
// identified by the socket's peer uid, so no credential is carried here:
// see docs/architecture.md §"Identity and credentials".
func storeClient(socket string) millv1connect.StoreServiceClient {
	return millv1connect.NewStoreServiceClient(unixHTTPClient(socket), socketURL)
}

// workerClient talks to a worker mounted beside `mill serve`, over the same
// unix socket `storeClient` reaches the store on. A `mill serve` with no
// `--work` in it mounts no route for this service at all, which answers
// CodeUnimplemented: see internal/server.New's doc comment.
func workerClient(socket string) millv1connect.WorkerServiceClient {
	return millv1connect.NewWorkerServiceClient(unixHTTPClient(socket), socketURL)
}

// unixHTTPClient dials socket whatever the request URL says, the transport
// storeClient and workerClient share.
func unixHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}
