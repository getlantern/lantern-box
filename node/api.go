package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
)

// registerRequest asks lantern-cloud for a route and the config to serve it
// with. The JSON names are lantern-cloud's PeerRegisterRequest fields.
type registerRequest struct {
	// ExternalIP is left empty unless the operator gives one, and lantern-cloud
	// then uses the address the request came from.
	ExternalIP   string `json:"external_ip,omitempty"`
	ExternalPort uint16 `json:"external_port"`
	InternalPort uint16 `json:"internal_port"`
	ServePool    bool   `json:"serve_pool"`
	// RunsUnboundedVolunteer says this lantern-box has the unbounded volunteer
	// service, so the config may include it.
	RunsUnboundedVolunteer bool `json:"runs_unbounded_volunteer"`
}

type registerResponse struct {
	RouteID                  string `json:"route_id"`
	ServerConfig             string `json:"server_config"`
	HeartbeatIntervalSeconds int64  `json:"heartbeat_interval_seconds"`
}

type lifecycleRequest struct {
	RouteID string `json:"route_id"`
}

type heartbeatRequest struct {
	RouteID string `json:"route_id"`
	// ActiveClients counts distinct client IPs with a connection open, so
	// lantern-cloud stops handing the node out once it carries its share.
	ActiveClients int `json:"active_clients"`
}

// apiError is a response outside 2xx, with the status and the start of its
// body.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("status %d: %s", e.status, e.body)
}

// isNotFound reports whether lantern-cloud no longer knows the route, so the
// node has to register again.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

// api is a client for lantern-cloud's /peer endpoints, the ones Share My
// Connection uses.
type api struct {
	httpClient *http.Client
	// baseURL includes the version segment, such as https://df.iantem.io/api/v1.
	baseURL  string
	deviceID string
	// userID, when set, lets the unbounded feature flag enroll the operator's
	// account.
	userID string
	// featureOverrides is sent as X-Lantern-Feature-Override, which staging
	// honors in place of the feature flags.
	featureOverrides string
	version          string
}

func (a *api) register(ctx context.Context, req registerRequest) (*registerResponse, error) {
	var resp registerResponse
	if err := a.do(ctx, "/peer/register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	if resp.RouteID == "" || resp.ServerConfig == "" {
		return nil, errors.New("register: response has no route or config")
	}
	return &resp, nil
}

// verify asks lantern-cloud to dial the node's inbound from outside. It fails
// unless the inbound is reachable, and lantern-cloud then drops the route.
func (a *api) verify(ctx context.Context, routeID string) error {
	if err := a.do(ctx, "/peer/verify", lifecycleRequest{RouteID: routeID}, nil); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

func (a *api) heartbeat(ctx context.Context, routeID string, activeClients int) error {
	if err := a.do(ctx, "/peer/heartbeat", heartbeatRequest{RouteID: routeID, ActiveClients: activeClients}, nil); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (a *api) deregister(ctx context.Context, routeID string) error {
	if err := a.do(ctx, "/peer/deregister", lifecycleRequest{RouteID: routeID}, nil); err != nil {
		return fmt.Errorf("deregister: %w", err)
	}
	return nil
}

func (a *api) do(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Lantern-Device-Id", a.deviceID)
	r.Header.Set("X-Lantern-App", "lantern-box")
	r.Header.Set("X-Lantern-Platform", runtime.GOOS)
	if a.version != "" {
		r.Header.Set("X-Lantern-Version", a.version)
	}
	if a.userID != "" {
		r.Header.Set("X-Lantern-User-Id", a.userID)
	}
	if a.featureOverrides != "" {
		r.Header.Set("X-Lantern-Feature-Override", a.featureOverrides)
	}
	resp, err := a.httpClient.Do(r)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{status: resp.StatusCode, body: string(bytes.TrimSpace(b))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
