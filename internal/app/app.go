// Package app wires the Skein server — config, database, every domain
// service, the HTTP handler, and the background workers — independent of how
// the process is run. cmd/skein and cmd/skein-desktop both call Build; they
// differ only in how they open a listener and how they decide to shut down.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/mridul249/Skein/internal/accounts"
	"github.com/mridul249/Skein/internal/auth"
	"github.com/mridul249/Skein/internal/config"
	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/db"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi"
	"github.com/mridul249/Skein/internal/httpapi/handlers"
	"github.com/mridul249/Skein/internal/logging"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/worker"
)

// Option customises Build. The only caller today is cmd/skein-desktop,
// wiring in its desktop OAuth connector — internal/app itself stays ignorant
// of what a "desktop connector" is beyond the interface handlers.Accounts
// already declares for it.
type Option func(*options)

type options struct {
	// desktopConnect is a factory rather than a built value: the connector
	// needs the *accounts.Service and *slog.Logger Build constructs
	// internally, which do not exist yet at the point a caller supplies
	// options, so Build calls this once they do.
	desktopConnect func(*accounts.Service, *slog.Logger) handlers.DesktopConnector

	// desktopClientID/Secret resolve the installed-app credentials used for
	// token refresh. Functions rather than values so an operator override via
	// the environment is read at use, matching the connector's behaviour.
	desktopClientID     func() string
	desktopClientSecret func() string
	useSQLite           bool
	sqlitePath          string
}

// WithDesktopConnect makes the accounts handler run the desktop OAuth flow
// (system browser, loopback listener, PKCE) instead of the web flow, and
// skips mounting the server-hosted OAuth callback route. Only
// cmd/skein-desktop passes this.
func WithDesktopConnect(newConnector func(*accounts.Service, *slog.Logger) handlers.DesktopConnector) Option {
	return func(o *options) { o.desktopConnect = newConnector }
}

// WithDesktopOAuth supplies the installed-app credentials used to REFRESH
// stored Drive tokens on the desktop build.
//
// This is separate from WithDesktopConnect because the two halves of the OAuth
// lifecycle take different paths, which is precisely how the 2026-08-05 bug
// happened: the connector builds a per-attempt config for the EXCHANGE, while
// refresh uses the config stored on accounts.Service. app.go built that stored
// config only from the web SKEIN_GOOGLE_CLIENT_ID/_SECRET/_REDIRECT_URL
// triple, which a desktop install never sets — so refresh had no credentials
// and every Drive operation died with oauth2: "unauthorized_client".
//
// Same resolver functions the connector uses, so the two cannot drift.
func WithDesktopOAuth(clientID, clientSecret func() string) Option {
	return func(o *options) {
		o.desktopClientID = clientID
		o.desktopClientSecret = clientSecret
	}
}

// WithSQLiteDatabase makes Build use the local SQLite desktop database instead
// of Postgres. Passing an empty path selects db.DesktopSQLitePath().
func WithSQLiteDatabase(path string) Option {
	return func(o *options) {
		o.useSQLite = true
		o.sqlitePath = path
	}
}

// App is a fully wired Skein server bound to a listener. The caller owns
// process lifecycle — signals for the headless build, window-close for the
// desktop build — and drives it through Serve and Shutdown.
type App struct {
	Config *config.Config
	Logger *slog.Logger

	// Auth and Files are exposed for operator tooling that drives the wired
	// services directly rather than over HTTP — see cmd/skein-recover, which
	// exists because the manifest routes are unreachable in a disaster: the
	// desktop server binds a random port and those routes need an operator
	// token a desktop install has no reason to have set.
	Auth  *auth.Service
	Files *files.Service

	closeDB  func()
	listener net.Listener
	httpSrv  *http.Server
	workers  *worker.Runner
}

// Build loads configuration, migrates, and wires every service, exactly as
// cmd/skein's main did before this package existed. It does not open a
// listener or start serving — that is Addr/Serve's job, so the caller
// chooses the bind address (a fixed one for the headless server, 127.0.0.1:0
// for the desktop build) before anything is listening.
func Build(ctx context.Context, opts ...Option) (*App, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	cfg, err := loadConfig(o)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	lg := logging.New(cfg.LogLevel, cfg.LogJSON)
	slog.SetDefault(lg)
	lg.Info("starting skein", slog.String("env", cfg.Env), slog.String("addr", cfg.Addr))

	if !cfg.EncryptionEnabled {
		lg.Warn("encryption at rest is DISABLED; file content will be stored in plaintext")
	}

	wired, err := selectPersistence(ctx, cfg, o, lg)
	if err != nil {
		return nil, err
	}

	masterKey, err := cfg.MasterKey()
	if err != nil {
		wired.close()
		return nil, err
	}
	keyring, err := skcrypto.NewKeyring(masterKey)
	if err != nil {
		wired.close()
		return nil, err
	}
	lg.Info("keyring ready", slog.String("key_id", keyring.KeyIDString()))

	// Known issue #48. Refuse a key that does not belong to this database, and
	// refuse it HERE — after the migrations, before any service exists and
	// before a single byte of user data is read.
	//
	// The placement is the feature. Without it the wrong key starts the server
	// perfectly and fails at the first download as a decryption error three
	// layers down, which reads as data corruption; the user concludes their
	// files are destroyed when they have simply restored the wrong key file.
	// Block 2 shipped export and recovery with a documented instruction to
	// compare two hex strings by eye, during a recovery, under stress. This is
	// that comparison, performed by the program.
	if wired.instance != nil {
		adopted, verr := wired.instance.VerifyMasterKeyID(ctx, keyring.KeyIDString())
		if verr != nil {
			wired.close()
			return nil, verr
		}
		if adopted {
			// Said out loud, because this is the ONE case the check cannot
			// protect: a database with no recorded key id accepts whatever key
			// it is first started with. That is unavoidable — you cannot
			// retroactively fingerprint a database created before
			// fingerprinting existed — so the defence is that an operator
			// restoring an older instance watches it happen rather than
			// finding out later. WARN rather than INFO: on a fresh install it
			// is noise seen once, and on a restore it is the line that matters.
			lg.Warn("no master key id was recorded; adopting the supplied key as this instance's",
				slog.String("key_id", keyring.KeyIDString()),
				slog.String("note", "expected on a first run; on a RESTORED database, "+
					"check this matches the Key ID in your exported key file"))
		}
	}

	authSvc := auth.NewService(
		wired.authStore,
		auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL),
		cfg.RefreshTokenTTL,
		lg.With(slog.String("component", "auth")),
	)

	var oauthCfg *oauth2.Config
	if cfg.GoogleConfigured() {
		oauthCfg = accounts.GoogleOAuthConfig(
			cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
	} else if shouldWarnAboutWebOAuth(o.desktopClientID != nil) {
		lg.Warn("google oauth is not configured; drives cannot be connected",
			slog.String("fix", "set SKEIN_GOOGLE_CLIENT_ID, _SECRET and _REDIRECT_URL"))
	}

	accountsSvc := accounts.NewService(
		wired.accountsStore,
		keyring,
		oauthCfg,
		lg.With(slog.String("component", "accounts")),
	)

	// No local fallback backend in a normal deployment: every shard belongs
	// to a connected drive, and silently writing to the server's own disk
	// when none is connected would be a surprise, not a convenience.
	// Desktop token refresh needs the installed-app credentials. Without this
	// the service refreshes against the web config — nil on a desktop install
	// — and every Drive call fails with unauthorized_client.
	if o.desktopClientID != nil && o.desktopClientSecret != nil {
		id, secret := o.desktopClientID(), o.desktopClientSecret()
		accountsSvc.SetDesktopOAuth(id, secret)
		if id == "" || secret == "" {
			lg.Warn("desktop oauth credentials are incomplete; " +
				"drive token refresh will fail until they are set")
		}
	}

	resolver := accounts.NewResolver(accountsSvc, nil)

	// Capacity is claimed through the atomic conditional UPDATE in
	// Architecture.md §5, never through a Go map. The reserver also owns the
	// janitor that reclaims what a crashed upload left behind.
	reserver := router.NewReserver(
		wired.routerStore,
		lg.With(slog.String("component", "router")),
	)

	// storedSize has to account for ciphertext expansion, or a reservation
	// covers the plaintext and the drive runs out at the final frame.
	storedSize := func(plain int64) int64 { return plain }
	if cfg.EncryptionEnabled {
		storedSize = skcrypto.StreamOverhead
	}

	planner := router.NewPlanner(
		reserver,
		router.Policy(cfg.RoutingPolicy),
		cfg.ShardSizeBytes,
		storedSize,
	)
	lg.Info("shard routing configured",
		slog.String("policy", cfg.RoutingPolicy),
		slog.Int64("shard_size_bytes", cfg.ShardSizeBytes),
		slog.Int64("frames_per_shard", cfg.ShardSizeBytes/skcrypto.FrameSize))

	filesSvc := files.NewService(
		wired.filesStore,
		files.NewStripingPlanner(planner, reserver),
		resolver,
		keyring,
		files.Config{
			Encrypt:        cfg.EncryptionEnabled,
			MaxUploadBytes: cfg.MaxUploadBytes,
		},
		lg.With(slog.String("component", "files")),
	)
	// One Drive worker pool per process, shared by quota sync and by the bulk
	// file operations. Two pools would each stay politely under the cap while
	// together exceeding it.
	filesSvc.SetWorkPool(accountsSvc.Pool())
	// Reconstruction scans every drive the user has connected, including
	// disabled ones — a disconnected drive still holds shards and manifests.
	filesSvc.SetAccountLister(accountsSvc)
	// After a recovery the account is bound to a freshly created app folder
	// while its shards sit in the old one; this lets reconstruction correct
	// that from what it actually found. See files.FolderRebinder.
	filesSvc.SetFolderRebinder(accountsSvc)
	// The durable identity manifests record, so a rebuilt database can still
	// claim them. Without this, recovery after losing the database finds
	// nothing — see the manifest UserEmail field.
	filesSvc.SetUserDirectory(authSvc)

	var desktopConnect handlers.DesktopConnector
	if o.desktopConnect != nil {
		desktopConnect = o.desktopConnect(accountsSvc, lg.With(slog.String("component", "desktopoauth")))
	}

	deps := httpapi.Deps{
		Config:         cfg,
		Logger:         lg,
		Health:         wired.health,
		Auth:           authSvc,
		Accounts:       accountsSvc,
		Files:          filesSvc,
		Keyring:        keyring,
		Dumper:         wired.dumper,
		DumpDB:         wired.dumpDB,
		DesktopConnect: desktopConnect,
	}
	// Desktop only: compiled out of the server binary entirely.
	wireDesktopDownloads(&deps, filesSvc, cfg.DownloadDir)

	srv, err := httpapi.New(deps)
	if err != nil {
		wired.close()
		return nil, fmt.Errorf("build server: %w", err)
	}

	// Background loops. Quota sync is never on the upload path: uploads read
	// the cached figure and rely on the atomic reservation to stay correct.
	workers := worker.New(lg.With(slog.String("component", "worker")),
		worker.Job{
			Name:       "quota-sync",
			Every:      cfg.QuotaSyncEvery,
			RunAtStart: true,
			Timeout:    5 * time.Minute,
			Run:        accountsSvc.SyncAll,
		},
		worker.Job{
			Name:    "purge-oauth-states",
			Every:   15 * time.Minute,
			Timeout: time.Minute,
			Run: func(ctx context.Context) error {
				_, perr := accountsSvc.PurgeExpiredOAuthStates(ctx)
				return perr
			},
		},
		// The reclaim janitor. Without it, a killed process strands its
		// reservation forever and the pool shrinks every time something
		// goes wrong.
		worker.Job{
			Name:       "reclaim-reservations",
			Every:      cfg.ReclaimEvery,
			RunAtStart: true,
			Timeout:    time.Minute,
			Run:        reserver.ReclaimExpired,
		},
		worker.Job{
			Name:    "purge-sessions",
			Every:   time.Hour,
			Timeout: time.Minute,
			Run: func(ctx context.Context) error {
				_, perr := authSvc.PurgeExpiredSessions(ctx)
				return perr
			},
		},
	)

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No WriteTimeout: responses stream multi-gigabyte file bodies and
		// a wall-clock write deadline would truncate them. Idle and header
		// timeouts still bound a slowloris.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return &App{
		Config:  cfg,
		Logger:  lg,
		Auth:    authSvc,
		Files:   filesSvc,
		closeDB: wired.close,
		httpSrv: httpSrv,
		workers: workers,
	}, nil
}

// Listen opens the configured TCP listener. The headless server passes
// through cfg.Addr; the desktop build overrides it with "127.0.0.1:0" so the
// OS assigns a free port, then reads it back from Addr().
func (a *App) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	a.listener = ln
	return nil
}

// Addr returns the address Listen bound to, including the OS-assigned port
// when the caller asked for port 0. Empty until Listen has been called.
func (a *App) Addr() string {
	if a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

// Serve starts the background workers and blocks serving HTTP until the
// listener closes or Shutdown is called. It returns nil on a clean shutdown
// (http.ErrServerClosed) and the underlying error otherwise. Listen must be
// called first.
func (a *App) Serve(ctx context.Context) error {
	if a.listener == nil {
		return fmt.Errorf("serve: Listen was not called")
	}
	a.workers.Start(ctx)
	a.Logger.Info("listening", slog.String("addr", a.Addr()))
	if err := a.httpSrv.Serve(a.listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Shutdown drains in-flight requests within the given context, stops the
// background workers, and releases the database pool. Safe to call once,
// after Serve has returned or from another goroutine to trigger the return.
func (a *App) Shutdown(ctx context.Context) error {
	err := a.httpSrv.Shutdown(ctx)

	// Background loops stop when their context is cancelled by the caller;
	// wait for them so the process does not exit mid-write. This blocks on
	// the caller's ctx being done, not on Shutdown's — Wait has no deadline
	// of its own, so a caller that never cancels its worker context would
	// hang here forever. Both cmd/skein and cmd/skein-desktop cancel the
	// context Serve/Start ran under before calling Shutdown.
	a.workers.Wait()

	a.closeDB()

	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

type persistence struct {
	authStore     auth.Store
	accountsStore accounts.Store
	routerStore   router.Store
	filesStore    files.Store
	health        httpapi.Health
	// dumper backs GET /api/system/backup, and knows which engine it is
	// dumping. dumpDB is the handle the schema version is read from; it is
	// nil under Postgres, where the version comes from pg_dump's own output
	// and the pool is pgx rather than database/sql.
	dumper *db.Dumper
	dumpDB *sql.DB
	// instance verifies the master key against the one this database was
	// created under (known issue #48). An interface so both engines' concrete
	// stores satisfy it without app importing either.
	instance interface {
		VerifyMasterKeyID(ctx context.Context, keyID string) (adopted bool, err error)
	}
	close func()
}

func loadConfig(o options) (*config.Config, error) {
	if o.useSQLite {
		return config.LoadDesktop()
	}
	return config.Load()
}

func openSQLitePersistence(ctx context.Context, sqlitePath string, lg *slog.Logger) (persistence, error) {
	path := sqlitePath
	if path == "" {
		var err error
		path, err = db.DesktopSQLitePath()
		if err != nil {
			return persistence{}, err
		}
	}
	sqlDB, err := db.OpenSQLite(ctx, path)
	if err != nil {
		return persistence{}, err
	}
	lg.Info("sqlite migrations applied", slog.String("path", path))
	return persistence{
		authStore:     auth.NewSQLiteStore(sqlDB),
		accountsStore: accounts.NewSQLiteStore(sqlDB),
		routerStore:   router.NewSQLiteStore(sqlDB),
		filesStore:    files.NewSQLiteStore(sqlDB),
		health:        sqliteHealth{db: sqlDB},
		dumper:        db.NewDumper(db.DialectSQLite, "", path),
		dumpDB:        sqlDB,
		instance:      db.NewSQLiteInstanceStore(sqlDB),
		close:         func() { _ = sqlDB.Close() },
	}, nil
}

func openPostgresPersistence(ctx context.Context, cfg *config.Config, lg *slog.Logger) (persistence, error) {
	if cfg == nil {
		return persistence{}, fmt.Errorf("internal error: nil config")
	}
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return persistence{}, err
	}
	lg.Info("migrations applied")

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return persistence{}, err
	}
	return persistence{
		authStore:     auth.NewPGStore(pool),
		accountsStore: accounts.NewPGStore(pool),
		routerStore:   router.NewPGStore(pool),
		filesStore:    files.NewPGStore(pool),
		health:        pool,
		dumper:        db.NewDumper(db.DialectPostgres, cfg.DatabaseURL, ""),
		instance:      db.NewPostgresInstanceStore(pool.Pool),
		close:         pool.Close,
	}, nil
}

func selectPersistence(ctx context.Context, cfg *config.Config, o options, lg *slog.Logger) (persistence, error) {
	if o.useSQLite {
		return openSQLitePersistence(ctx, o.sqlitePath, lg)
	}
	return openPostgresPersistence(ctx, cfg, lg)
}

type sqliteHealth struct{ db *sql.DB }

func (h sqliteHealth) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	return nil
}

// shouldWarnAboutWebOAuth reports whether the "set SKEIN_GOOGLE_CLIENT_ID,
// _SECRET and _REDIRECT_URL" warning applies to this build.
//
// SERVER ONLY. The desktop build never sets the web credentials and does not
// need them: it connects drives over loopback PKCE using the
// SKEIN_GOOGLE_DESKTOP_* pair, which has its own warning. Printing the web
// variables on a desktop run sends the reader to fix three settings that would
// change nothing for them, immediately next to a second warning naming the two
// that would.
//
// Found 2026-08-06 by following docs/SETUP.md from a clean environment exactly
// as written, which is what that page exists for. Package-level rather than
// inline because a log message is precisely the kind of thing that drifts
// unnoticed, and Build's logger has no seam a test can reach.
func shouldWarnAboutWebOAuth(isDesktopBuild bool) bool { return !isDesktopBuild }
