package main

import (
	"context"
	"log"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/caarlos0/env/v6"
	oidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// Define an application struct to hold the application-wide dependencies for the
// web application.
type application struct {
	errorLog                *log.Logger
	logger                  *zerolog.Logger
	ACLMap                  ACLMap
	proxy                   *httputil.ReverseProxy
	verifier                *oidc.IDTokenVerifier
	Debug                   bool          `env:"DEBUG" envDefault:"false"`
	UpstreamURL             *url.URL      `env:"UPSTREAM_URL,required"`
	OptimizeExpressions     bool          `env:"OPTIMIZE_EXPRESSIONS" envDefault:"true"`
	SafeMode                bool          `env:"SAFE_MODE" envDefault:"true"`
	SetProxyHeaders         bool          `env:"SET_PROXY_HEADERS" envDefefault:"false"`
	ACLPath                 string        `env:"ACL_PATH" envDefault:"./acl.yaml"`
	OIDCRealmURL            string        `env:"OIDC_REALM_URL,required"`
	OIDCClientID            string        `env:"OIDC_CLIENT_ID,required"`
	Port                    int           `env:"PORT" envDefault:"8080"`
	ReadTimeout             time.Duration `env:"READ_TIMEOUT" envDefault:"10s"`
	WriteTimeout            time.Duration `env:"WRITE_TIMEOUT" envDefault:"10s"`
	GracefulShutdownTimeout time.Duration `env:"GRACEFUL_SHUTDOWN_TIMEOUT" envDefault:"20s"`
}

type contextKey string

const contextKeyHasFullaccess = contextKey("hasFullaccess")
const contextKeyLabelFilter = contextKey("labelFilter")

func main() {
	logWrapper := stdErrorLogWrapper{logger: &zlog.Logger}
	errorLog := log.New(logWrapper, "", log.Lshortfile)

	app := &application{
		logger:   &zlog.Logger,
		errorLog: errorLog,
	}

	zerolog.CallerMarshalFunc = app.lshortfile

	err := env.Parse(app)
	if err != nil {
		app.logger.Fatal().Caller().
			Err(err).Msgf("")
	}

	if app.Debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	app.ACLMap, err = app.loadACL()
	if err != nil {
		app.logger.Fatal().Caller().
			Err(err).Msgf("")
	}

	app.logger.Info().Caller().
		Msgf("Connecting to OIDC backend (%q)", app.OIDCRealmURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, app.OIDCRealmURL)
	if err != nil {
		app.logger.Fatal().Caller().
			Err(err).Msgf("")
	}

	oidcConfig := &oidc.Config{
		ClientID: app.OIDCClientID,
	}
	app.verifier = provider.Verifier(oidcConfig)

	app.proxy = httputil.NewSingleHostReverseProxy(app.UpstreamURL)
	// TODO: somehow pass more context to ErrorLog
	app.proxy.ErrorLog = app.errorLog
	app.proxy.FlushInterval = time.Millisecond * 200

	err = app.serve()
	if err != nil {
		app.logger.Fatal().Caller().
			Err(err).Msgf("")
	}
}
