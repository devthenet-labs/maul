// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/telemetry"
	"github.com/devthenet-labs/maul/internal/version"
	"github.com/devthenet-labs/maul/internal/web"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
	"github.com/devthenet-labs/maul/internal/web/credstore"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the status page server",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the maul resources live in (default: POD_NAMESPACE)")
	f.String("agent-namespace", "maul-agents",
		"namespace the agent Jobs run in; live transcripts are followed from their pod logs")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("auth-config", "",
		"path to the mounted authentication config; absent means rollup statistics only, no findings access")
	f.Bool("intents-enabled", false,
		"serve the intents views (board, timeline, run panel) with per-Project access reviews; needs auth mode "+
			"oidc with usernamePrefix and groupsPrefix, and turns on the hardened browser envelope")
	f.Bool("usage-enabled", false,
		"serve the usage views (a signed-in person's own usage, and everyone's for holders of get on the "+
			"virtual usage/all); needs auth mode oidc as the intents views do plus Dex's federated:id scope, "+
			"since a person is their GitHub id; turns on the hardened browser envelope")
	f.Bool("account-enabled", false,
		"serve the Account page (a signed-in person sets, replaces and deletes their own model credential, "+
			"write-only, behind an access review on the virtual modelcredentials/self); needs auth mode oidc as the "+
			"usage views do, the credential store the chart renders under egressBroker.userCredentials.mode, and "+
			"turns on the hardened browser envelope")
	f.String("credentials-namespace", v1alpha1.DefaultCredentialsNamespace,
		"the namespace the Account page writes credential Secrets to (write-only)")
	f.String("account-providers", string(v1alpha1.ModelCredentialProviderAnthropic),
		"comma-separated providers the Account page offers a credential for, in order: anthropic (claude) "+
			"and openai (codex); the chart sets the providers its enabled runners use")
	f.Bool("agents-paused", false,
		"show the operator's agent pause (the chart's agent.paused) on the signed-in page; display only, the job "+
			"controllers enforce it")
	f.Bool("intents-dev-insecure", false,
		"development only: let the intents views run in auth mode none, which shows every Project to every "+
			"visitor; refused unless --listen-addr is a loopback address")
	return cmd
}

// intentsWiring is what --intents-enabled adds to the server, decided before
// anything starts.
type intentsWiring struct {
	on bool
	// projects is the tier source the auth gate chose; nil means the
	// access-review granter, built once the manager's client exists.
	projects web.ProjectGranter
}

func newIntentsWiring(opts *cli.Options, authCfg *auth.Config) (intentsWiring, error) {
	if !opts.Bool("intents-enabled") {
		return intentsWiring{}, nil
	}
	projects, err := intentsAuthGate(authCfg, opts.Bool("intents-dev-insecure"), opts.ListenAddr)
	if err != nil {
		return intentsWiring{}, err
	}
	return intentsWiring{on: true, projects: projects}, nil
}

// viewsWiring is the optional views the flags turn on, each checked against
// the auth posture before anything starts.
type viewsWiring struct {
	intents intentsWiring
	usage   usageWiring
	account accountWiring
}

func newViewsWiring(opts *cli.Options, authCfg *auth.Config) (viewsWiring, error) {
	intents, err := newIntentsWiring(opts, authCfg)
	if err != nil {
		return viewsWiring{}, err
	}
	usage, err := newUsageWiring(opts.Bool("usage-enabled"), authCfg)
	if err != nil {
		return viewsWiring{}, err
	}
	account, err := newAccountWiring(opts.Bool("account-enabled"), opts.String("credentials-namespace"),
		opts.StringList("account-providers"), authCfg)
	if err != nil {
		return viewsWiring{}, err
	}
	return viewsWiring{intents: intents, usage: usage, account: account}, nil
}

// registerIndexes installs the indexes of every view that is on.
func (vw viewsWiring) registerIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if err := vw.intents.registerIndexes(ctx, idx); err != nil {
		return err
	}
	return vw.usage.registerIndexes(ctx, idx)
}

// apply turns on every view that is on. reader reads uncached.
func (vw viewsWiring) apply(srv *web.Server, c client.Client, reader client.Reader, namespace string,
	tailer web.Tailer) *web.Server {
	srv = vw.usage.apply(vw.intents.apply(srv, c, namespace, tailer), c, namespace)
	return vw.account.apply(srv, c, reader, namespace)
}

// accountWiring is what --account-enabled adds to the server, decided
// before anything starts.
type accountWiring struct {
	on                   bool
	credentialsNamespace string
	providers            []v1alpha1.ModelCredentialProvider
}

// newAccountWiring checks the auth posture may serve the Account page: as
// the usage views, oidc with the federated:id scope, since a credential is
// kept under the person's GitHub id. There is no development bypass.
func newAccountWiring(enabled bool, credentialsNamespace string, providers []string, authCfg *auth.Config) (
	accountWiring, error) {
	if !enabled {
		return accountWiring{}, nil
	}
	if err := authCfg.RequireForUsage(); err != nil {
		return accountWiring{}, fmt.Errorf("--account-enabled: %w", err)
	}
	if errs := validation.IsDNS1123Label(credentialsNamespace); len(errs) > 0 {
		return accountWiring{}, fmt.Errorf("--credentials-namespace %q: %s", credentialsNamespace,
			strings.Join(errs, "; "))
	}
	ps, err := accountProviders(providers)
	if err != nil {
		return accountWiring{}, err
	}
	return accountWiring{on: true, credentialsNamespace: credentialsNamespace, providers: ps}, nil
}

// accountProviders reads --account-providers: one or more of the
// providers a person may store a credential for, each once.
func accountProviders(raw []string) ([]v1alpha1.ModelCredentialProvider, error) {
	out := make([]v1alpha1.ModelCredentialProvider, 0, len(raw))
	for _, r := range raw {
		p := v1alpha1.ModelCredentialProvider(strings.TrimSpace(r))
		if !slices.Contains(v1alpha1.ModelCredentialProviders, p) {
			return nil, fmt.Errorf("--account-providers: %q is not one of %v", r, v1alpha1.ModelCredentialProviders)
		}
		if slices.Contains(out, p) {
			return nil, fmt.Errorf("--account-providers: %s is listed twice", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--account-providers: name at least one of %v", v1alpha1.ModelCredentialProviders)
	}
	return out, nil
}

// apply turns the Account page on in srv; with it off it returns srv
// unchanged. Records are read uncached (no informer, so no list/watch on
// them), Secrets only ever written.
func (aw accountWiring) apply(srv *web.Server, c client.Client, reader client.Reader, namespace string) *web.Server {
	if !aw.on {
		return srv
	}
	return srv.WithAccount(web.AccountOptions{
		Self: authz.NewSubresourceReviewer(c, namespace, v1alpha1.GroupVersion.Group,
			web.AccountSelfResource, web.AccountSelfSubresource, 0),
		Store: &credstore.Store{Client: c, Reader: reader, Namespace: namespace,
			CredentialsNamespace: aw.credentialsNamespace},
		Providers: aw.providers,
	})
}

// usageWiring is what --usage-enabled adds to the server, decided before
// anything starts.
type usageWiring struct{ on bool }

// newUsageWiring checks the auth posture may serve the usage views: oidc as
// for the intents views, plus the federated:id scope that carries a
// person's GitHub id. There is no development bypass: without a GitHub id
// no one has usage to see.
func newUsageWiring(enabled bool, authCfg *auth.Config) (usageWiring, error) {
	if !enabled {
		return usageWiring{}, nil
	}
	if err := authCfg.RequireForUsage(); err != nil {
		return usageWiring{}, fmt.Errorf("--usage-enabled: %w", err)
	}
	return usageWiring{on: true}, nil
}

// registerIndexes installs the usage index; the UsageRollup informer it
// implies needs the usage RBAC, which the chart grants only with the views
// on.
func (uw usageWiring) registerIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if !uw.on {
		return nil
	}
	return web.RegisterUsageIndexes(ctx, idx)
}

// apply turns the usage views on in srv; with them off it returns srv
// unchanged.
func (uw usageWiring) apply(srv *web.Server, c client.Client, namespace string) *web.Server {
	if !uw.on {
		return srv
	}
	return srv.WithUsage(web.UsageOptions{All: authz.NewSubresourceReviewer(c, namespace,
		v1alpha1.GroupVersion.Group, web.UsageAllResource, web.UsageAllSubresource, 0)})
}

// registerIndexes installs the intents indexes. The intents kinds are cached
// only with the views on: their informers need the intents RBAC, which the
// chart grants only then.
func (iw intentsWiring) registerIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if !iw.on {
		return nil
	}
	return web.RegisterIntentIndexes(ctx, idx)
}

// apply turns the intents views on in srv; with them off it returns srv
// unchanged.
func (iw intentsWiring) apply(srv *web.Server, c client.Client, namespace string, tailer web.Tailer) *web.Server {
	if !iw.on {
		return srv
	}
	projects := iw.projects
	if projects == nil {
		projects = authz.NewProjectReviewer(c, namespace, 0)
	}
	o := web.IntentsOptions{Projects: projects}
	if t, ok := tailer.(*jobs.Tailer); ok && t != nil {
		o.Facts = t
	}
	return srv.WithIntents(o)
}

// intentsAuthGate checks the auth posture may serve the intents views. Mode
// oidc must pass RequireForIntents; mode none is accepted only behind the
// explicit development flag and a loopback listen address. It returns the
// tier source the server uses.
func intentsAuthGate(cfg *auth.Config, devInsecure bool, listen string) (web.ProjectGranter, error) {
	if cfg != nil && cfg.Mode == auth.ModeNone {
		if !devInsecure {
			return nil, errors.New("--intents-enabled refuses auth mode none, which shows every Project to " +
				"every visitor; use mode oidc (or --intents-dev-insecure on a loopback address for development)")
		}
		if !loopback(listen) {
			return nil, fmt.Errorf("--intents-dev-insecure needs a loopback --listen-addr, not %q", listen)
		}
		return authz.FullProjects{}, nil
	}
	if err := cfg.RequireForIntents(); err != nil {
		return nil, err
	}
	return nil, nil
}

// loopback reports whether a listen address binds only the loopback
// interface: an explicit 127.0.0.0/8 or ::1 host, or localhost. An empty
// host (":8080") binds every interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// agentTailer builds the read-only pod-log reader for the agent namespace.
// It is a plain clientset rather than the manager's client: pod logs are a
// subresource stream, not a cacheable object, and the manager's cache is
// scoped to the maul namespace anyway.
func agentTailer(kubeconfig, agentNamespace string) (web.Tailer, error) {
	if agentNamespace == "" {
		return nil, errors.New("no agent namespace configured")
	}
	cfg, err := kube.RestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset: %w", err)
	}
	return jobs.NewTailer(cs, agentNamespace), nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("MAUL_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "status-server",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return errors.New("namespace is required (--namespace or POD_NAMESPACE)")
	}

	authCfg, err := auth.LoadConfig(opts.String("auth-config"))
	if err != nil {
		return err
	}
	if authCfg == nil {
		log.LogAttrs(ctx, slog.LevelWarn,
			"authentication not configured; serving rollup statistics only — findings and actions are unavailable")
	}
	// Checked before anything starts: a posture that cannot serve the
	// intents views safely must not come up serving them.
	views, err := newViewsWiring(opts, authCfg)
	if err != nil {
		return err
	}
	authn, err := auth.New(ctx, authCfg, log)
	if err != nil {
		return err
	}

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig: opts.String("kubeconfig"),
		Namespaces: []string{namespace},
		HealthAddr: opts.String("health-addr"),
		Log:        log,
	})
	if err != nil {
		return err
	}

	// The finding-detail projection looks child runs up by owning finding;
	// the index must exist before the cache starts serving.
	if err := web.RegisterIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := views.registerIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}

	// Mode none bypasses access reviews entirely; every other posture asks
	// the cluster per identity.
	var granter web.Granter = authz.NewReviewer(mgr.GetClient(), namespace, 0)
	if authCfg != nil && authCfg.Mode == auth.ModeNone {
		granter = authz.Full{}
	}

	srv := web.NewServer(mgr.GetClient(), namespace, authn, granter, log).WithAgentsPaused(opts.Bool("agents-paused"))
	// Transcripts read through the uncached API reader (caching every
	// ConfigMap in the namespace to serve objects read once per page view is
	// not worth an informer) and follow live runs from the agent namespace's
	// pod logs, via the API server — the status server never dials an agent pod.
	agentNS := opts.String("agent-namespace")
	tailer, err := agentTailer(opts.String("kubeconfig"), agentNS)
	if err != nil {
		// Live following is a nicety; persisted transcripts still serve.
		log.LogAttrs(ctx, slog.LevelWarn, "live transcripts unavailable",
			slog.String("agent_namespace", agentNS), slog.Any("error", err))
	}
	srv = views.apply(srv.WithTranscripts(mgr.GetAPIReader(), tailer), mgr.GetClient(), mgr.GetAPIReader(),
		namespace, tailer)

	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return srv.StartWatch(ctx, mgr.GetCache())
	}))
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	mode := "unconfigured"
	if authCfg != nil {
		mode = authCfg.Mode
	}
	log.LogAttrs(ctx, slog.LevelInfo, "status-server starting",
		slog.String("namespace", namespace),
		slog.String("listen_addr", httpSrv.Addr),
		slog.String("auth_mode", mode),
		slog.Bool("intents", views.intents.on),
		slog.Bool("usage", views.usage.on),
		slog.Bool("account", views.account.on))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return mgr.Start(ctx) })
	g.Go(func() error {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	})
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
