// Package testenv starts envtest with the controller manager and hatchet-lite containers for tests.
package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/controller"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
)

// ErrNoAssets is returned when KUBEBUILDER_ASSETS is not set.
var ErrNoAssets = errors.New("KUBEBUILDER_ASSETS is not set; run with " +
	"KUBEBUILDER_ASSETS=\"$(go tool -modfile=tools/go.mod setup-envtest use -p path)\"")

// RepoRoot returns the repository root directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Scheme returns a scheme with the core and hatchet-controller types.
func Scheme() *k8sruntime.Scheme {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(hatchetv1alpha1.AddToScheme(scheme))
	return scheme
}

// Environment returns an envtest environment with the controller CRDs, or ErrNoAssets.
func Environment() (*envtest.Environment, error) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return nil, ErrNoAssets
	}
	return &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                Scheme(),
	}, nil
}

// Env is a running envtest API server with both reconcilers.
type Env struct {
	Config *rest.Config
	// Client is an uncached client.
	Client client.Client
	// Clock drives token rotation of the tenant reconciler.
	Clock *clocktesting.FakePassiveClock

	env    *envtest.Environment
	cancel context.CancelFunc
	done   chan error

	mu          sync.Mutex
	afterCreate func(hatchet.Tenant) error
	failRevoke  bool
}

// StartEnvtest starts envtest and a manager running both reconcilers built with the
// real constructors and ClientFactory. NetworkPolicy management is enabled.
func StartEnvtest() (*Env, error) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	env, err := Environment()
	if err != nil {
		return nil, err
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("starting envtest: %w", err)
	}
	e := &Env{Config: cfg, env: env, Clock: clocktesting.NewFakePassiveClock(time.Now())}
	if err := e.startManager(); err != nil {
		_ = env.Stop()
		return nil, err
	}
	return e, nil
}

func (e *Env) startManager() error {
	scheme := Scheme()
	c, err := client.New(e.Config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	e.Client = c

	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {Label: labels.SelectorFromSet(labels.Set{
					hatchetv1alpha1.ManagedByLabel: hatchetv1alpha1.ControllerName,
				})},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	if err := controller.NewHatchetInstanceReconciler(mgr).SetupWithManager(mgr); err != nil {
		cancel()
		return err
	}
	tenants := controller.NewHatchetTenantReconciler(mgr, e.wrapFactory(controller.NewClientFactory(mgr.GetClient())), true)
	tenants.Clock = e.Clock
	if err := tenants.SetupWithManager(ctx, mgr); err != nil {
		cancel()
		return err
	}

	e.done = make(chan error, 1)
	go func() { e.done <- mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		return errors.New("manager cache did not sync")
	}
	return nil
}

// Stop stops the manager and the API server.
func (e *Env) Stop() error {
	e.cancel()
	err := <-e.done
	return errors.Join(err, e.env.Stop())
}

// FailAfterCreate makes the next successful tenant:create return an error to the controller,
// simulating a failure between tenant:create and the annotation write-back. The created
// tenant is passed to fn.
func (e *Env) FailAfterCreate(fn func(hatchet.Tenant)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.afterCreate = func(t hatchet.Tenant) error {
		fn(t)
		return errors.New("injected failure after tenant:create")
	}
}

// FailNextRevoke makes the next RevokeAPIToken call fail without reaching Hatchet.
func (e *Env) FailNextRevoke() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failRevoke = true
}

// ResetFailures clears injected failures that have not fired.
func (e *Env) ResetFailures() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.afterCreate = nil
	e.failRevoke = false
}

func (e *Env) wrapFactory(f controller.ClientFactory) controller.ClientFactory {
	return func(ctx context.Context, instance *hatchetv1alpha1.HatchetInstance) (hatchet.Client, error) {
		c, err := f(ctx, instance)
		if err != nil {
			return nil, err
		}
		return &injectingClient{Client: c, env: e}, nil
	}
}

// injectingClient forwards to the real client and applies FailAfterCreate and FailNextRevoke.
type injectingClient struct {
	hatchet.Client
	env *Env
}

func (c *injectingClient) CreateTenant(ctx context.Context, name, slug string) (hatchet.Tenant, error) {
	t, err := c.Client.CreateTenant(ctx, name, slug)
	if err != nil {
		return t, err
	}
	c.env.mu.Lock()
	hook := c.env.afterCreate
	c.env.afterCreate = nil
	c.env.mu.Unlock()
	if hook != nil {
		return hatchet.Tenant{}, hook(t)
	}
	return t, nil
}

func (c *injectingClient) RevokeAPIToken(ctx context.Context, tenantID, tokenID string) error {
	c.env.mu.Lock()
	fail := c.env.failRevoke
	c.env.failRevoke = false
	c.env.mu.Unlock()
	if fail {
		return errors.New("injected failure on token revoke")
	}
	return c.Client.RevokeAPIToken(ctx, tenantID, tokenID)
}
