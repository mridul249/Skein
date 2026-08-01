//go:build production

package app

import (
	"context"
	"net/url"

	"github.com/wailsapp/wails/v2/internal/binding"
	"github.com/wailsapp/wails/v2/internal/frontend/desktop"
	"github.com/wailsapp/wails/v2/internal/frontend/dispatcher"
	"github.com/wailsapp/wails/v2/internal/frontend/runtime"
	"github.com/wailsapp/wails/v2/internal/logger"
	"github.com/wailsapp/wails/v2/internal/menumanager"
	"github.com/wailsapp/wails/v2/pkg/options"
)

func (a *App) Run() error {
	err := a.frontend.Run(a.ctx)
	a.frontend.RunMainLoop()
	a.frontend.WindowClose()
	if a.shutdownCallback != nil {
		a.shutdownCallback(a.ctx)
	}
	return err
}

// CreateApp creates the app!
//
// Skein fork: startURL, when non-empty, is threaded into the frontend's
// context under the (unexported-elsewhere, internal-only) "starturl" key
// that desktop.NewFrontend already reads. When set, NewFrontend skips
// registering the custom wails:// scheme handler entirely and navigates the
// window straight to this URL instead — a real HTTP address served by an
// in-process net/http.Server, not the synthetic-request bridge that has no
// cancellable context and no working download handling on Linux. See
// third_party/wails-v2.13.0/PATCH.md for the full rationale.
func CreateApp(appoptions *options.App, startURL string) (*App, error) {
	var err error

	ctx := context.Background()
	if startURL != "" {
		u, uerr := url.Parse(startURL)
		if uerr != nil {
			return nil, uerr
		}
		ctx = context.WithValue(ctx, "starturl", u)
	}

	// Merge default options
	options.MergeDefaults(appoptions)

	debug := IsDebug()
	devtoolsEnabled := IsDevtoolsEnabled()
	ctx = context.WithValue(ctx, "debug", debug)
	ctx = context.WithValue(ctx, "devtoolsEnabled", devtoolsEnabled)

	// Set up logger
	myLogger := logger.New(appoptions.Logger)
	if IsDebug() {
		myLogger.SetLogLevel(appoptions.LogLevel)
	} else {
		myLogger.SetLogLevel(appoptions.LogLevelProduction)
	}
	ctx = context.WithValue(ctx, "logger", myLogger)
	ctx = context.WithValue(ctx, "obfuscated", IsObfuscated())

	// Preflight Checks
	err = PreflightChecks(appoptions, myLogger)
	if err != nil {
		return nil, err
	}

	// Create the menu manager
	menuManager := menumanager.NewManager()

	// Process the application menu
	if appoptions.Menu != nil {
		err = menuManager.SetApplicationMenu(appoptions.Menu)
		if err != nil {
			return nil, err
		}
	}

	// Create binding exemptions - Ugly hack. There must be a better way
	bindingExemptions := []interface{}{
		appoptions.OnStartup,
		appoptions.OnShutdown,
		appoptions.OnDomReady,
		appoptions.OnBeforeClose,
	}
	appBindings := binding.NewBindings(myLogger, appoptions.Bind, bindingExemptions, IsObfuscated(), appoptions.EnumBind)
	eventHandler := runtime.NewEvents(myLogger)
	ctx = context.WithValue(ctx, "events", eventHandler)
	// Attach logger to context
	if debug {
		ctx = context.WithValue(ctx, "buildtype", "debug")
	} else {
		ctx = context.WithValue(ctx, "buildtype", "production")
	}

	messageDispatcher := dispatcher.NewDispatcher(ctx, myLogger, appBindings, eventHandler, appoptions.ErrorFormatter, appoptions.DisablePanicRecovery)
	appFrontend := desktop.NewFrontend(ctx, appoptions, myLogger, appBindings, messageDispatcher)
	eventHandler.AddFrontend(appFrontend)

	ctx = context.WithValue(ctx, "frontend", appFrontend)
	result := &App{
		ctx:              ctx,
		frontend:         appFrontend,
		logger:           myLogger,
		menuManager:      menuManager,
		startupCallback:  appoptions.OnStartup,
		shutdownCallback: appoptions.OnShutdown,
		debug:            debug,
		devtoolsEnabled:  devtoolsEnabled,
		options:          appoptions,
	}

	return result, nil

}
