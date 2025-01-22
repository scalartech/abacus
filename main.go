package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/alicebob/miniredis/v2"

	"github.com/anandvarma/namegen"

	"github.com/redis/go-redis/v9"

	"github.com/jasonlovesdoggo/abacus/middleware"

	"github.com/gin-contrib/cors"
	analytics "github.com/tom-draper/api-analytics/analytics/go/gin"

	"github.com/jasonlovesdoggo/abacus/utils"

	"github.com/gin-gonic/gin"
)

const (
	DocsUrl string = "http://localhost:8080/abacus/"
	Version string = "1.3.3"
)

var (
	Client          *redis.Client
	RateLimitClient *redis.Client
	DbNum           = 0 // 0-16
	StartTime       time.Time
	Shard           string
)

func init() {
	utils.LoadEnv()
	// Use miniredis for testing
	if strings.ToLower(os.Getenv("TESTING")) == "true" {
		setupMockRedis()
		return
	}

	// Production Redis setup
	Shard = namegen.New().Get()

	if strings.ToLower(os.Getenv("DEBUG")) == "true" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	ADDR := os.Getenv("REDIS_HOST") + ":" + os.Getenv("REDIS_PORT")
	log.Println("Listening to redis on: " + ADDR)
	DbNum, _ = strconv.Atoi(os.Getenv("REDIS_DB"))

	Client = redis.NewClient(&redis.Options{
		Addr:     ADDR, // Redis server address
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       DbNum,
	})
	RateLimitClient = redis.NewClient(&redis.Options{
		Addr:     ADDR, // Redis server address
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       DbNum + 1,
	})
}

func setupMockRedis() {
	// Used for testing, "miniredis" is a mock Redis server that runs in-memory for testing purposes only (no persistence)
	mr, err := miniredis.Run()
	if err != nil {
		log.Fatalf("Failed to start miniredis: %v", err)
	}

	log.Println("Using miniredis for testing")

	// Connect clients to miniredis
	Client = redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	RateLimitClient = redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
}

func CreateRouter() *gin.Engine {
	utils.InitializeStatsManager(Client)
	r := gin.Default()
	// Cors
	corsConfig := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
		AllowCredentials: false,
		AllowAllOrigins:  true,
		MaxAge:           12 * time.Hour,
	}
	r.Use(cors.New(corsConfig))
	r.Use(gin.Recovery()) // recover from panics and returns a 500 error
	if os.Getenv("API_ANALYTICS_ENABLED") == "true" {
		r.Use(analytics.Analytics(os.Getenv("API_ANALYTICS_KEY"))) // Add middleware
		log.Println("Analytics enabled")
	}
	route := r.Group("")
	route.Use(middleware.Stats())
	if os.Getenv("RATE_LIMIT_ENABLED") == "true" {
		route.Use(middleware.RateLimit(RateLimitClient))
		log.Println("Rate limiting enabled")
	}
	// Define routes
	r.NoRoute(func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, DocsUrl)
	})
	// heath check
	r.StaticFile("/favicon.svg", "./assets/favicon.svg")
	r.StaticFile("/favicon.ico", "./assets/favicon.ico")

	{ // Stats Routes
		route.GET("/healthcheck", func(context *gin.Context) {
			context.JSON(http.StatusOK, gin.H{
				"status": "ok", "uptime": time.Since(StartTime).String()})
		})

		route.GET("/docs", func(context *gin.Context) {
			context.Redirect(http.StatusPermanentRedirect, DocsUrl)
		})

		route.GET("/stats", StatsView)
	}
	{ // Public Routes
		route.GET("/get/:namespace/*key", GetView)

		route.GET("/hit/:namespace/*key", HitView)
		route.GET("/stream/:namespace/*key", middleware.SSEMiddleware(), StreamValueView)

		route.POST("/create/:namespace/*key", CreateView)
		route.GET("/create/:namespace/*key", CreateView)

		route.GET("/create/", CreateRandomView)
		route.POST("/create/", CreateRandomView)

		route.GET("/info/:namespace/*key", InfoView)
	}
	authorized := route.Group("")
	authorized.Use(middleware.Auth(Client))
	{ // Authorized Routes
		authorized.POST("/delete/:namespace/*key", DeleteView)

		authorized.POST("/set/:namespace/*key", SetView)
		authorized.POST("/reset/:namespace/*key", ResetView)
		authorized.POST("/update/:namespace/*key", UpdateByView)
	}
	return r
}

func main() {
	//gin.SetMode(gin.ReleaseMode)
	// only run the following if .env is present
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	utils.LoadEnv()
	StartTime = time.Now()
	// Initialize the Gin router
	r := CreateRouter()
	// Set up autocert manager for SSL
	certManager := autocert.Manager{
        Prompt:     autocert.AcceptTOS,
        HostPolicy: autocert.HostWhitelist("countr.click", "www.countr.click"),
        Cache:      autocert.DirCache("certs"),
    }

	// Configure the HTTPS server
	srv := &http.Server{
        Addr:    ":https",
        Handler: r,
        TLSConfig: &tls.Config{
            GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
                log.Printf("TLS handshake from %s, SNI: %s, ALPN: %v", 
                    hello.Conn.RemoteAddr(), hello.ServerName, hello.SupportedProtos)
                
                // Allow acme-tls/1 for Let's Encrypt challenges
                var nextProtos []string
                for _, p := range hello.SupportedProtos {
                    if p == "acme-tls/1" {
                        nextProtos = []string{"acme-tls/1"}
                        break
                    }
                }
                if nextProtos == nil {
                    nextProtos = []string{"h2", "http/1.1"}
                }

                return &tls.Config{
                    GetCertificate: certManager.GetCertificate,
                    NextProtos:     nextProtos,
                }, nil
            },
            MinVersion: tls.VersionTLS12,
        },
    }

	// HTTP server to handle ACME challenges and redirect to HTTPS
	go func() {
		httpSrv := &http.Server{
			Addr:    ":http",
			Handler: certManager.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target := "https://" + r.Host + r.URL.Path
				if len(r.URL.RawQuery) > 0 {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
			})),
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server error: %v\n", err)
		}
	}()

	// Start HTTPS server
	go func() {
        log.Printf("Starting HTTPS server on port 443")
        if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Printf("HTTPS listen error: %s\n", err)
            // Print more details about the error
            if err, ok := err.(*tls.CertificateVerificationError); ok {
                log.Printf("Certificate verification error: %v\n", err)
                for _, cert := range err.UnverifiedCertificates {
                    log.Printf("Unverified certificate: %s\n", cert.Subject)
                }
            }
        }
    }()

	// Wait for interrupt signal to gracefully shutdown the server with
	// a timeout of 5 seconds.
	quit := make(chan os.Signal, 1)
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall. SIGKILL but can"t be catch, so don't need add it
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	close(utils.ServerClose)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}
	select {
	case <-ctx.Done():
		log.Println("timeout of 5 seconds.")
	}
	log.Println("Server exiting")
}
