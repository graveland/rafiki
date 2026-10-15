// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/webui"
)

// webShutdownGrace bounds the graceful stop of the proxy: long enough for an
// in-flight request to finish, short enough that a wedged client cannot stall
// the command's exit.
const webShutdownGrace = 5 * time.Second

// newWebCmd builds `rafiki web`.
func newWebCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the rafiki web UI on localhost and open it in a browser",
		Args:  cobra.NoArgs,
		RunE:  runWeb,
	}
	cmd.Flags().Int("port", 0, "port to serve on (0 = any free port)")
	cmd.Flags().Bool("no-open", false, "do not open a browser")
	cmd.Flags().String("dev-assets", "", "reverse-proxy static assets to this URL (e.g. http://localhost:5173)")
	return cmd
}

// runWeb serves the browser UI through a 127.0.0.1-only proxy in front of the
// daemon's Connect control plane, and blocks until the process is signalled.
//
// The upstream is resolved lazily (webResolver), the way the proxy wants it:
// the daemon is not dialled until the browser's first Control request, and a
// tokenless remote profile is answered as "sign in" rather than dialled at all.
func runWeb(cmd *cobra.Command, _ []string) error {
	devAssets, _ := cmd.Flags().GetString("dev-assets")
	port, _ := cmd.Flags().GetInt("port")
	noOpen, _ := cmd.Flags().GetBool("no-open")

	// A Go-only build embeds only dist/.gitkeep, so there is nothing to serve;
	// --dev-assets (a Vite dev server) is the one way to run without `make web`.
	if devAssets == "" && !webui.HasUI() {
		return errors.New("this rafiki was built without the web UI (make web)")
	}

	p, err := resolveProfile(cmd)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	port = ln.Addr().(*net.TCPAddr).Port

	cfg := webui.Config{Port: port, Resolve: webResolver(p)}
	if devAssets != "" {
		u, err := url.Parse(devAssets)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("parse --dev-assets %q: %w", devAssets, err)
		}
		cfg.DevAssets = u
	} else {
		cfg.Assets = webui.Assets()
	}

	s := webui.New(cfg)
	s.EnableLogin(webLogin{p: p})

	uiURL := fmt.Sprintf("http://127.0.0.1:%d/#k=%s", port, s.LaunchCode())
	fmt.Fprintln(cmd.OutOrStdout(), "rafiki web:", uiURL)
	if !noOpen {
		openBrowser(uiURL)
	}

	srv := s.HTTPServer()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	<-cmdCtx(cmd).Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), webShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// webResolver resolves the proxy's upstream from an already-resolved profile.
// A tokenless remote profile — the one state that can only ever answer
// Unauthenticated — is mapped to webui.ErrNoCredential, so the UI reads it as
// "sign in" and never a daemon error. Every other resolve error (a socket that
// does not exist, say) passes through unchanged.
func webResolver(p profile.Resolved) webui.Resolver {
	return func() (webui.Upstream, error) {
		ep, err := connectEndpointFor(p)
		if errors.Is(err, errRemoteNoToken) {
			return webui.Upstream{}, fmt.Errorf("%w: %v", webui.ErrNoCredential, err)
		}
		if err != nil {
			return webui.Upstream{}, err
		}
		return webui.Upstream{Client: ep.httpClient, BaseURL: ep.baseURL}, nil
	}
}

// webLogin implements webui.LoginBackend against the daemon's Login service.
// Begin and Complete each derive their endpoint afresh from the (pre-login,
// tokenless) profile — the Login service is served outside authentication, so
// the same profile the control plane refuses is exactly the one it answers.
// Errors pass through unwrapped: the Connect code is what the browser renders.
type webLogin struct{ p profile.Resolved }

func (w webLogin) Begin(ctx context.Context, redirectPort uint32) (string, string, uint32, error) {
	resp, err := loginEndpointFor(w.p).login().BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{
		RedirectPort: redirectPort,
		ClientHost:   clientHost(),
	}))
	if err != nil {
		return "", "", 0, err
	}
	return resp.Msg.GetLoginId(), resp.Msg.GetAuthorizeUrl(), resp.Msg.GetRedirectPort(), nil
}

func (w webLogin) Complete(ctx context.Context, loginID, callbackQuery string) (webui.Upstream, error) {
	resp, err := completeLogin(ctx, loginEndpointFor(w.p), w.p, loginID, callbackQuery)
	if err != nil {
		return webui.Upstream{}, err
	}
	// The freshly minted token is now in the profile; rebuild the control-plane
	// endpoint over it so the proxy switches to an authenticated upstream.
	p2 := w.p
	p2.Token = resp.GetToken()
	ep, err := connectEndpointFor(p2)
	if err != nil {
		return webui.Upstream{}, err
	}
	return webui.Upstream{Client: ep.httpClient, BaseURL: ep.baseURL}, nil
}
