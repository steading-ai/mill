// Package gateway is the credential gateway: the host-side reverse proxy a
// session's model requests go through so that the session holds the step
// token and the host holds the subscription (docs/sandbox.md §Credentials,
// docs/architecture.md §"Identity and credentials").
//
// A session is given ANTHROPIC_BASE_URL naming this gateway and
// ANTHROPIC_AUTH_TOKEN carrying its step token. The gateway checks the bearer
// against the token the run loop last rotated in for a live step — the same
// hash comparison the workspace socket makes — refuses anything else with
// 401, swaps the bearer for the upstream credential, and forwards. It sees
// every response, so it counts what the model billed from outside the
// session, one ledger row per response with source `gateway`, and it is
// where a step's token budget and a workflow's model pin are enforced.
//
// It is not a general proxy: the Messages API paths are forwarded and
// everything else is 404.
package gateway
