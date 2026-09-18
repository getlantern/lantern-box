package outboundeval

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/ntp"
)

// ErrNoAssignment means the server has nothing for this runner to measure at
// the moment. It is the steady state of a fleet with no evaluation running, not
// a fault, and it is resolved by asking again later.
var ErrNoAssignment = errors.New("no outbound evaluation assignment available")

// maxControlResponseBytes bounds a control API response, which carries an
// assignment or an attestation and nothing large.
const maxControlResponseBytes int64 = 1 << 20

// apiError is a control API response outside 2xx.
type apiError struct {
	status int
}

func (e apiError) Error() string {
	return fmt.Sprintf("control API returned HTTP status %d", e.status)
}

// retryable reports whether the same request may succeed unchanged later. A
// refusal the server states in 4xx is not retryable: it takes a different
// request, or a credential the runner does not yet hold.
func (e apiError) retryable() bool {
	return e.status == http.StatusTooManyRequests || e.status >= http.StatusInternalServerError
}

// apiClient talks to the control API over one outbound. Every call travels that
// outbound, including attestation, whose source address the server attributes
// the measurement to.
type apiClient struct {
	http           *http.Client
	requestTimeout time.Duration
}

func newAPIClient(ctx context.Context, out A.Outbound, requestTimeout time.Duration) *apiClient {
	return &apiClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return out.DialContext(ctx, network, M.ParseSocksaddr(address))
				},
				TLSClientConfig: &tls.Config{
					Time:    ntp.TimeFuncFromContext(ctx),
					RootCAs: A.RootPoolFromContext(ctx),
				},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		requestTimeout: requestTimeout,
	}
}

func (c *apiClient) close() {
	c.http.CloseIdleConnections()
}

// acquire asks for one assignment, returning ErrNoAssignment when the server
// has nothing to measure.
func (c *apiClient) acquire(ctx context.Context, endpoint, token string, request AssignmentRequest) (Assignment, error) {
	var assignment Assignment
	err := c.post(ctx, endpoint, token, request, &assignment)
	var status apiError
	if errors.As(err, &status) && status.status == http.StatusServiceUnavailable {
		return Assignment{}, ErrNoAssignment
	}
	if err != nil {
		return Assignment{}, fmt.Errorf("acquire assignment: %w", err)
	}
	return assignment, nil
}

// attest exchanges one window's challenge for an attestation token. It carries
// no bearer credential: the challenge is the authorization.
func (c *apiClient) attest(ctx context.Context, endpoint string, request AttestationRequest) (Attestation, error) {
	var attestation Attestation
	if err := c.post(ctx, endpoint, "", request, &attestation); err != nil {
		return Attestation{}, fmt.Errorf("attest window: %w", err)
	}
	if err := boundedIdentifier("attestation token", attestation.Token, maxTokenBytes); err != nil {
		return Attestation{}, err
	}
	return attestation, nil
}

// submit delivers one report, authorized by the report token it carries. A
// conflict means the server already holds this report, which is success.
func (c *apiClient) submit(ctx context.Context, endpoint string, report Report) error {
	err := c.post(ctx, endpoint, "", report, nil)
	var status apiError
	if errors.As(err, &status) && status.status == http.StatusConflict {
		return nil
	}
	if err != nil {
		return fmt.Errorf("submit report: %w", err)
	}
	return nil
}

func (c *apiClient) post(ctx context.Context, endpoint, token string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		// Draining leaves the connection reusable for the retry.
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, maxControlResponseBytes))
		return apiError{status: httpResponse.StatusCode}
	}
	if response == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, maxControlResponseBytes))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(httpResponse.Body, maxControlResponseBytes)).Decode(response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
