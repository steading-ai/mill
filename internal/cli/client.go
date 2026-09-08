package cli

import (
	"context"
	"net"
	"net/http"

	"github.com/ivy/mill/gen/mill/v1/millv1connect"
)

// socketURL is the base every request is made against. The host is never
// resolved — the transport below dials the unix socket whatever it says —
// but Connect requires a well-formed URL to build a path from.
const socketURL = "http://mill"

// storeClient talks to `mill serve` over its unix socket. The caller is
// identified by the socket's peer uid, so no credential is carried here:
// see docs/architecture.md §"Identity and credentials".
func storeClient(socket string) millv1connect.StoreServiceClient {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return millv1connect.NewStoreServiceClient(client, socketURL)
}
