// Package wails is the main package of the Wails project.
// It is used by client applications.
package wails

import (
	_ "github.com/wailsapp/wails/v2/internal/goversion" // Add Compile-Time version check for minimum go version
	"github.com/wailsapp/wails/v2/pkg/application"
	"github.com/wailsapp/wails/v2/pkg/options"
)

// Run creates an application based on the given config and executes it
func Run(options *options.App) error {
	mainApp := application.NewWithOptions(options)
	return mainApp.Run()
}

// RunWithStartURL is Run, except the window navigates directly to startURL
// (a real http:// address) instead of the wails:// custom scheme. Skein
// fork — see internal/app/app_production.go's CreateApp doc comment.
func RunWithStartURL(opts *options.App, startURL string) error {
	mainApp := application.NewWithOptionsAndStartURL(opts, startURL)
	return mainApp.Run()
}
