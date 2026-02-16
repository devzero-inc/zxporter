package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// HealthResponse represents the JSON structuure for /healthz and /readyz responses
type HealthResponse struct {
	Status     string                       `json:"status"`
	Error      string                       `json:"message,omitempty"`
	Components map[string]ComponentResponse `json:"components,omitempty"`
}

// ComponentResponse represents the JSON structure for /components/{component} responses
type ComponentResponse struct {
	Status   string            `json:"status"`
	Message  string            `json:"message,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// HealthServer serves health and readiness endpoints
type HealthServer struct {
	manager *HealthManager
	addr    string
	server  *http.Server
}

// NewHealthServer creates a new HealthServer bound to the specified address
func NewHealthServer(manager *HealthManager, addr string) *HealthServer {
	s := &HealthServer{
		manager: manager,
		addr:    addr,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthzHandler)
	mux.HandleFunc("/readyz", s.readyzHandler)
	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Start begins serving health endpoints
func (s *HealthServer) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	go s.server.Serve(ln)
	return nil
}

// Stop gracefully shuts down the server
func (s *HealthServer) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// healthzHandler handles the /healthz endpoint
func (s *HealthServer) healthzHandler(w http.ResponseWriter, r *http.Request) {
	report := s.manager.BuildReport()
	err := s.manager.LivenessCheck(r)

	response := HealthResponse{
		Components: buildComponentResponses(report),
	}
	if err != nil {
		response.Status = "unhealthy"
		response.Error = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	response.Status = "healthy"
	writeJSON(w, http.StatusOK, response)

}

// readyzHandler handles the /readyz endpoint
func (s *HealthServer) readyzHandler(w http.ResponseWriter, r *http.Request) {
	report := s.manager.BuildReport()
	err := s.manager.ReadinessCheck(r)

	response := HealthResponse{
		Components: buildComponentResponses(report),
	}
	if err != nil {
		response.Status = "not ready"
		response.Error = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	response.Status = "ready"
	writeJSON(w, http.StatusOK, response)
}

// buildComponentResponses converts the health report into a map of component responses
func buildComponentResponses(report map[string]ComponentStatus) map[string]ComponentResponse {
	components := make(map[string]ComponentResponse, len(report))
	for name, status := range report {
		components[name] = ComponentResponse{
			Status:   status.Status.String(),
			Message:  status.Message,
			Metadata: status.Metadata,
		}
	}
	return components
}

// writeJSON writes the response as JSON
func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}
