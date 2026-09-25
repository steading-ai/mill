package cli

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/workspace"
)

// The credential gateway's settings (docs/sandbox.md §Credentials). Off, a
// run behaves as it always has: the session reaches the model on the
// account's own login. On, every session is pointed at the gateway with its
// step token as bearer, and the gateway holds the subscription credential
// the session never sees.
const (
	gatewayUsage = "put the credential gateway in front of every session: " +
		"ANTHROPIC_BASE_URL names it and ANTHROPIC_AUTH_TOKEN carries the step token (docs/sandbox.md §Credentials)"
	gatewayListenUsage   = "address the credential gateway listens on: the host side of the machine network"
	gatewayUpstreamUsage = "the API the credential gateway forwards to"
	modelCredentialUsage = "file holding the subscription credential the gateway presents upstream, " +
		"the output of `claude setup-token`; never read from the environment (default: $" +
		gateway.CredentialsDirectoryEnv + "/" + gateway.CredentialName + ")"
)

// gatewayOptions is the gateway's share of a run's flags.
type gatewayOptions struct {
	enabled        bool
	listen         string
	upstream       string
	credentialFile string
}

// bindGatewayFlags declares the gateway's flags on a command.
func bindGatewayFlags(cmd *cobra.Command, opts *gatewayOptions) {
	f := cmd.Flags()
	f.BoolVar(&opts.enabled, "gateway", false, gatewayUsage)
	f.StringVar(&opts.listen, "gateway-listen", gateway.DefaultListen, gatewayListenUsage)
	f.StringVar(&opts.upstream, "gateway-upstream", gateway.DefaultUpstream, gatewayUpstreamUsage)
	f.StringVar(&opts.credentialFile, "model-credential-file", gateway.DefaultCredentialPath(), modelCredentialUsage)
}

// applyGatewayConfig fills in the gateway flags the command line left
// alone from the configuration file, for a command that does not go
// through applyConfig.
func applyGatewayConfig(cmd *cobra.Command, config Config, opts *gatewayOptions) {
	if config.Gateway && unset(cmd, "gateway") {
		opts.enabled = true
	}
	for name, target := range map[string]*string{
		"gateway-listen": &opts.listen, "gateway-upstream": &opts.upstream, "model-credential-file": &opts.credentialFile,
	} {
		value := map[string]string{
			"gateway-listen": config.GatewayListen, "gateway-upstream": config.GatewayUpstream, "model-credential-file": config.ModelCredentialFile,
		}[name]
		if value != "" && unset(cmd, name) {
			*target = value
		}
	}
}

// startGateway builds and starts the gateway the configuration asks for,
// or nothing. It runs once per process, before any runner is built: one
// listener serves every run the process drives, and a credential file that
// is missing refuses here, before a node is claimed.
func startGateway(cmd *cobra.Command, opts gatewayOptions) (*gateway.Server, error) {
	if !opts.enabled {
		return nil, nil //nolint:nilnil // no gateway is a legitimate answer: the run goes on the account's login
	}
	credential, err := gateway.LoadCredential(opts.credentialFile)
	if err != nil {
		return nil, fmt.Errorf("mill: --gateway: %w", err)
	}
	g, err := gateway.New(gateway.Config{
		Upstream:   opts.upstream,
		Credential: credential,
		Log:        slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return nil, fmt.Errorf("mill: --gateway-upstream: %w", err)
	}
	server, err := gateway.Listen(opts.listen, g)
	if err != nil {
		return nil, fmt.Errorf("mill: --gateway-listen: %w", err)
	}
	return server, nil
}

// gatewayRunOptions is what a runner takes from a started gateway: the
// gateway itself, and the workspace decision that points sessions at it.
func gatewayRunOptions(server *gateway.Server, options *run.Options) {
	if server == nil {
		return
	}
	options.Gateway = server.Gateway()
	options.Egress = workspace.Egress{Gateway: server.URL()}
}

// credentialAge is how long ago the subscription credential was minted, as
// `model_credential_minted` in the configuration file records it. A
// `claude setup-token` lasts a year, and the CLI's own expiry warning
// applies only to `/login` credentials, so this is the only warning an
// operator gets.
const (
	modelCredentialMintedKey = "model_credential_minted"
	credentialWarnAfter      = 11 // months
)

// credentialWarning is the doctor's warning when the credential is older
// than eleven months, or empty. A date that does not parse is a warning
// too: the operator wrote one down to be warned.
func credentialWarning(minted string, now time.Time) string {
	if minted == "" {
		return fmt.Sprintf("%s is not set in the configuration file, so its age cannot be checked", modelCredentialMintedKey)
	}
	when, err := time.Parse(time.DateOnly, minted)
	if err != nil {
		return fmt.Sprintf("%s %q is not a date (YYYY-MM-DD)", modelCredentialMintedKey, minted)
	}
	if now.Before(when.AddDate(0, credentialWarnAfter, 0)) {
		return ""
	}
	return fmt.Sprintf("the credential was minted %s, more than %d months ago; a setup-token lasts a year — mint a new one",
		minted, credentialWarnAfter)
}
