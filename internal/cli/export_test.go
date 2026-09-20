package cli

import "net/http"

// SetForgeHTTPClient stands a spec's recorded GitHub in for the one host
// the adapter reaches, for the length of the spec.
func SetForgeHTTPClient(client *http.Client) (restore func()) {
	previous := forgeHTTP
	forgeHTTP = client
	return func() { forgeHTTP = previous }
}
