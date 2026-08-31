package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// loadDotEnv reads a .env file (if present) into the process environment.
// Deliberately not a dependency — this is a handful of lines, and pulling
// in github.com/joho/godotenv for this alone isn't worth it. Real
// environment variables (e.g. set by Render at deploy time) always win —
// this only fills in gaps, never overrides something already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env file — fine in production, where real env vars are set directly
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if _, alreadySet := os.LookupEnv(key); !alreadySet {
			os.Setenv(key, val)
		}
	}
}

// Config holds all environment-driven settings. See .env.example for the
// full list and what each one is for.
type Config struct {
	Port string
	Env  string

	SupabaseURL        string
	SupabaseAnonKey    string
	SupabaseServiceKey string
	SupabaseJWTSecret  string
	SupabaseDBURL      string

	RedisURL string

	// PaymentProvider selects which deposit-collection provider is active
	// ("daraja" | "palpluss"). Defaults to daraja — see loadConfig below
	// and payment.go's NewPaymentProvider. Flipping this back and forth is
	// the whole point of the abstraction: both clients stay fully
	// configured regardless of which one is selected.
	PaymentProvider string

	DarajaEnv            string
	DarajaConsumerKey    string
	DarajaConsumerSecret string
	DarajaShortcode      string
	DarajaPasskey        string
	DarajaCallbackURL    string

	PalplussEnv string
	// PalplussChannelIDs is the pool of channel IDs available to choose
	// from — PALPLUSS_CHANEL_ID is comma-separated (e.g. "id1,id2") to
	// support that. Which one is actually used for new deposits is the
	// *active* channel, which is admin-switchable at runtime from the
	// dashboard (see palpluss.go's PalplussClient.SetActiveChannel and
	// admin.go's SetPalplussChannel) but lives only in memory — it is not
	// persisted, so it resets to the first entry here on every
	// restart/redeploy. This list is just the set of valid choices.
	PalplussChannelIDs     []string
	PalplussAPIKey         string
	PalplussBasicAuthToken string
	PalplussCallbackURL    string

	GameHouseEdge float64
}

func loadConfig() Config {
	edge, err := strconv.ParseFloat(getenv("GAME_HOUSE_EDGE", "0.03"), 64)
	if err != nil {
		edge = 0.03
	}
	return Config{
		Port: getenv("PORT", "8080"),
		Env:  getenv("ENV", "development"),

		SupabaseURL:        os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:    os.Getenv("SUPABASE_ANON_KEY"),
		SupabaseServiceKey: os.Getenv("SUPABASE_SERVICE_KEY"),
		SupabaseJWTSecret:  os.Getenv("SUPABASE_JWT_SECRET"),
		SupabaseDBURL:      os.Getenv("SUPABASE_DB_URL"),

		RedisURL: os.Getenv("REDIS_URL"),

		PaymentProvider: strings.ToLower(getenv("PAYMENT_PROVIDER", "daraja")),

		DarajaEnv:            getenv("DARAJA_ENV", "sandbox"),
		DarajaConsumerKey:    os.Getenv("DARAJA_CONSUMER_KEY"),
		DarajaConsumerSecret: os.Getenv("DARAJA_CONSUMER_SECRET"),
		DarajaShortcode:      os.Getenv("DARAJA_SHORTCODE"),
		DarajaPasskey:        os.Getenv("DARAJA_PASSKEY"),
		DarajaCallbackURL:    os.Getenv("DARAJA_CALLBACK_URL"),

		PalplussEnv:            getenv("PALPLUSS_ENV", "sandbox"),
		PalplussChannelIDs:     splitAndTrim(os.Getenv("PALPLUSS_CHANEL_ID"), ","),
		PalplussAPIKey:         os.Getenv("PALPLUSS_API_KEY"),
		PalplussBasicAuthToken: os.Getenv("PALPLUSS_BASIC_AUTH_TOKEN"),
		PalplussCallbackURL:    os.Getenv("PALPLUSS_CALLBACK_URL"),

		GameHouseEdge: edge,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitAndTrim splits a delimited env var into a clean list — trims
// whitespace around each entry (so "id1, id2" and "id1,id2" behave the
// same) and drops empty entries (so a trailing comma or an unset var
// doesn't produce a bogus "" channel). Returns nil for an empty input.
func splitAndTrim(s, sep string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// App holds every dependency HTTP handlers need. auth.go, wallet.go,
// admin.go, and influencer.go are all methods on *App; game.go's fast-path
// handlers live on *GameEngine instead since they only ever need db/rdb/hub,
// not the full App (keeps the Redis-only fast path honest about what it
// touches).
type App struct {
	cfg Config
	db  *DB
	rdb *RDB
	// payments is whichever deposit-collection provider is active (see
	// payment.go / server.go's loadConfig PAYMENT_PROVIDER). Both
	// DarajaClient and PalplussClient satisfy this interface — only one is
	// constructed and assigned here, but a.game and everything else stays
	// unaware of which.
	payments   PaymentProvider
	httpClient *http.Client
	game       *GameEngine
	hub        *WSHub
	jwks       keyfunc.Keyfunc // verifies Supabase-issued JWTs — see middleware.go
}

func main() {
	loadDotEnv(".env")
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := NewDB(ctx, cfg.SupabaseDBURL)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer db.Close()

	rdb, err := NewRDB(cfg.RedisURL)
	if err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}
	defer rdb.Close()

	jwks, err := newJWKS(cfg.SupabaseURL)
	if err != nil {
		log.Fatalf("failed to load Supabase JWKS: %v", err)
	}

	hub := NewWSHub()
	game := NewGameEngine(db, rdb, hub, cfg.GameHouseEdge)

	app := &App{
		cfg:        cfg,
		db:         db,
		rdb:        rdb,
		payments:   NewPaymentProvider(cfg),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		game:       game,
		hub:        hub,
		jwks:       jwks,
	}
	log.Printf("payments: using %s as the active deposit provider", app.payments.Name())

	// Round engine + write-behind persistence worker run for the lifetime
	// of the process, independent of any single HTTP request.
	go game.Run(ctx)
	go game.RunPersistWorker(ctx)

	router := app.newRouter()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("dotpesa backend listening on :%s (env=%s)", cfg.Port, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func (a *App) newRouter() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://gopesa.onrender.com", "https://gopesa-1.onrender.com", "http://localhost:8082"}, // tighten to the real frontend origin before production
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Dependency-free keep-alive target — see spec §4.3. Pinged externally
	// every 10 minutes to stop Render's free tier from spinning the
	// instance down between rounds.
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Round-state broadcast — plain WS, one-way, see ws.go.
	r.Get("/ws", a.hub.ServeWS)

	// Static admin/influencer/debug pages served directly by this service —
	// intentionally not proxied through the public frontend (spec: "live in
	// the backend served statically ... about them being exposed").
	fileServer := http.FileServer(http.Dir("./static"))
	r.Handle("/admin/*", http.StripPrefix("/admin/", fileServer))

	r.Route("/api", func(r chi.Router) {
		authLimiter := a.RateLimit(10, time.Minute)

		r.Route("/auth", func(r chi.Router) {
			r.With(authLimiter).Post("/signup", a.Signup)
			r.With(authLimiter).Post("/login", a.Login)
			r.With(authLimiter).Post("/admin/login", a.AdminLogin)
			r.With(authLimiter).Post("/password-reset/request", a.RequestPasswordReset)

			r.Group(func(r chi.Router) {
				r.Use(a.AuthMiddleware)
				r.Get("/profile", a.GetOwnProfile)
				r.Patch("/profile", a.UpdateOwnProfile)
			})
		})

		r.Route("/game", func(r chi.Router) {
			r.Get("/state", a.game.GetState)
			r.Get("/history", a.game.GetHistory)

			r.Group(func(r chi.Router) {
				r.Use(a.AuthMiddleware)
				r.Post("/bet", a.game.PlaceBet)
				r.Post("/cashout", a.game.Cashout)

				r.With(a.RequireDebugAccess).Get("/admin/round-debug", a.game.AdminRoundDebug)
			})
		})

		r.Route("/wallet", func(r chi.Router) {
			r.Post("/daraja/callback", a.DarajaCallback)     // unauthenticated Safaricom webhook
			r.Post("/palpluss/callback", a.PalplussCallback) // unauthenticated PalPluss webhook

			r.Group(func(r chi.Router) {
				r.Use(a.AuthMiddleware)
				r.Get("/balance", a.GetWalletBalance)
				r.Get("/transactions", a.GetWalletTransactions)
				r.With(a.RateLimit(5, time.Minute)).Post("/deposit/mpesa", a.InitiateDeposit)
				r.With(a.RateLimit(5, time.Minute)).Post("/withdraw", a.InitiateWithdrawal)
			})
		})

		r.Route("/influencer", func(r chi.Router) {
			r.Use(a.AuthMiddleware)
			r.Use(RequireRole("influencer"))
			r.Get("/mpesa/balance", a.GetInfluencerMpesaBalance)
			r.Post("/mpesa/withdraw", a.MockMpesaWithdraw)
			r.Post("/withdraw", a.InfluencerWithdraw)
			r.Get("/transactions", a.GetInfluencerTransactions)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(a.AuthMiddleware)
			r.Use(RequireRole("admin"))
			r.Get("/stats", a.GetAdminStats)
			r.Get("/users", a.ListUsers)
			r.Patch("/users/{id}/role", a.UpdateUserRole)
			r.Patch("/users/{id}/debug-access", a.UpdateDebugAccess)
			r.Get("/withdrawals/pending", a.ListPendingWithdrawals)
			r.Post("/withdrawals/review", a.ReviewWithdrawal)
			r.Get("/transactions", a.ListTransactions)
			r.Get("/influencer-withdrawals", a.ListInfluencerWithdrawals)
			r.Post("/influencer-withdrawals/{id}/{action}", a.MarkInfluencerWithdrawal)
			r.Get("/payment/palpluss-channels", a.GetPalplussChannels)
			r.Patch("/payment/palpluss-channel", a.SetPalplussChannel)
		})
	})

	return r
}
