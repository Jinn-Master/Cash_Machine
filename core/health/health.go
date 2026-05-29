package health

// core/health/health.go
//
// HTTP health check endpoint for monitoring.
// Exposes /health (liveness) and /ready (readiness) endpoints.
//
// Liveness: always returns 200 if the process is running.
// Readiness: returns 200 only if the RPC connection is alive and
//            the last heartbeat is within the staleness threshold.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	// Max time since last heartbeat before we report not ready
	heartbeatStalenessThreshold = 5 * time.Minute
	// HTTP server port for health checks
	defaultPort = 8080
)

type Status struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Checks    Checks    `json:"checks"`
}

type Checks struct {
	RPCAlive        bool      `json:"rpc_alive"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
	HeartbeatAge    string    `json:"heartbeat_age"`
	WalletBalanceETH float64  `json:"wallet_balance_eth"`
}

type Server struct {
	client   *ethclient.Client
	wallet   string
	mu       sync.RWMutex
	lastBeat time.Time
	port     int
	server   *http.Server
}

func New(client *ethclient.Client, walletAddr string, port int) *Server {
	if port == 0 {
		port = defaultPort
	}
	return &Server{
		client: client,
		wallet: walletAddr,
		port:   port,
	}
}

// UpdateHeartbeat should be called by the main loop on each successful cycle.
func (s *Server) UpdateHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastBeat = time.Now()
}

// Start begins serving health check endpoints in a background goroutine.
func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Log but don't crash — health endpoint is non-critical
			fmt.Printf("health server error: %v\n", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutdownCtx)
	}()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Status{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lastBeat := s.lastBeat
	s.mu.RUnlock()

	// Check RPC is alive
	rpcAlive := false
	header, err := s.client.HeaderByNumber(r.Context(), nil)
	if err == nil && header != nil {
		rpcAlive = true
	}

	beatAge := time.Since(lastBeat)
	ready := rpcAlive && beatAge < heartbeatStalenessThreshold

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(Status{
		Status:    map[bool]string{true: "ready", false: "not_ready"}[ready],
		Timestamp: time.Now().UTC(),
		Checks: Checks{
			RPCAlive:      rpcAlive,
			LastHeartbeat: lastBeat,
			HeartbeatAge:  beatAge.Round(time.Second).String(),
		},
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lastBeat := s.lastBeat
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "# HELP last_heartbeat_seconds Time since last heartbeat\n")
	fmt.Fprintf(w, "# TYPE last_heartbeat_seconds gauge\n")
	fmt.Fprintf(w, "last_heartbeat_seconds %.0f\n", time.Since(lastBeat).Seconds())
	fmt.Fprintf(w, "# HELP rpc_connected Whether the RPC connection is alive\n")
	fmt.Fprintf(w, "# TYPE rpc_connected gauge\n")

	rpcAlive := 0
	if _, err := s.client.HeaderByNumber(r.Context(), nil); err == nil {
		rpcAlive = 1
	}
	fmt.Fprintf(w, "rpc_connected %d\n", rpcAlive)
}
