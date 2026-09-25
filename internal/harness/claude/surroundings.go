package claude

import (
	"fmt"
	"strings"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/workspace"
)

// systemPrompt is what the adapter appends to the harness's own system
// prompt: the mill's facts about the session's surroundings, as distinct
// from the step's instructions, which are the prompt. It is composed from
// the workspace's decisions and nothing else, so a workspace that decided
// nothing — the local backend without a proxy — adds nothing and the
// session runs under the harness's prompt alone.
//
// The one fact today is egress. A session in a machine has no direct route
// out: the host's nftables table drops everything from the machine but the
// proxy and the gateway, so a connection that ignores the environment hangs
// to its timeout with nothing naming why. An agent debugging that reaches
// for the right explanation only if it has been told (docs/sandbox.md
// §Network), and the mill skill mounted beside it says the same at length.
func systemPrompt(session harness.Session) string {
	proxy := session.Workspace.Env[workspace.HTTPSProxyVar]
	gateway := session.Workspace.Env[workspace.ModelGatewayVar]
	if proxy == "" && gateway == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("This session runs where the mill put it, and the mill decided its way out. ")
	if proxy != "" {
		fmt.Fprintf(&b, "Every outbound connection goes through the forward proxy named by %s and %s in the environment (%s), with %s listing what bypasses it; tools that honour those variables reach out, and a direct connection that ignores them is dropped by the host and times out by design, so a connection that hangs is one that did not use the proxy, not a network fault to work around. ",
			workspace.HTTPSProxyVar, workspace.HTTPProxyVar, proxy, workspace.NoProxyVar)
	}
	if gateway != "" {
		fmt.Fprintf(&b, "The model is reached through the gateway named by %s (%s), presenting %s as the bearer; that token is the step's and is retired when the step ends. ",
			workspace.ModelGatewayVar, gateway, workspace.ModelGatewayTokenVar)
	}
	b.WriteString("The mill skill at ~/.claude/skills/mill/SKILL.md documents this and the rest of the machine.")
	return b.String()
}
