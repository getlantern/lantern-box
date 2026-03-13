package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/getlantern/lantern-box/tracker/datacap"
	"github.com/getlantern/lantern-box/tracker/datacap/dcpb"
)

// grpcDataCapAPI implements datacap.DataCapAPI using generated gRPC types.
// The proto types in dcpb/ mirror lantern-cloud's DataCapService definition,
// keeping lantern-cloud out of lantern-box's dependency graph.
type grpcDataCapAPI struct {
	client dcpb.DataCapServiceClient
}

func newGRPCDataCapAPI(conn grpc.ClientConnInterface) *grpcDataCapAPI {
	return &grpcDataCapAPI{
		client: dcpb.NewDataCapServiceClient(conn),
	}
}

func (g *grpcDataCapAPI) ReportUsage(ctx context.Context, batch *datacap.UsageBatch) (*datacap.ReportUsageResult, error) {
	records := make([]*dcpb.DataCapUsage, 0, len(batch.Records))
	for _, r := range batch.Records {
		records = append(records, &dcpb.DataCapUsage{
			DeviceId:    r.DeviceID,
			BytesUsed:   r.BytesUsed,
			CapLimit:    r.CapLimit,
			Platform:    r.Platform,
			CountryCode: r.CountryCode,
		})
	}

	resp, err := g.client.ReportUsage(ctx, &dcpb.DataCapUsageBatch{Records: records})
	if err != nil {
		return nil, fmt.Errorf("gRPC ReportUsage: %w", err)
	}

	result := &datacap.ReportUsageResult{
		Results: make([]datacap.UsageResultEntry, 0, len(resp.GetResults())),
	}
	if resp != nil {
		for _, r := range resp.Results {
			result.Results = append(result.Results, datacap.UsageResultEntry{
				DeviceID: r.DeviceId,
				Success:  r.Success,
				Error:    r.Error,
			})
		}
	}
	return result, nil
}

func (g *grpcDataCapAPI) SyncDeviceState(ctx context.Context, deviceID string) (*datacap.DeviceState, error) {
	resp, err := g.client.SyncDeviceState(ctx, &dcpb.DataCapSyncRequest{
		DeviceId: deviceID,
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC SyncDeviceState: %w", err)
	}

	return &datacap.DeviceState{
		DeviceID:    resp.DeviceId,
		BytesUsed:   resp.BytesUsed,
		CapLimit:    resp.CapLimit,
		ExpiryTime:  resp.ExpiryTime,
		CountryCode: resp.CountryCode,
		Platform:    resp.Platform,
	}, nil
}

// datacapMTLSConfig holds paths to mTLS credential files for the datacap gRPC connection.
type datacapMTLSConfig struct {
	CACertPath     string
	ClientCertPath string
	ClientKeyPath  string
}

// newDataCapGRPCConn creates a gRPC client connection for the datacap cloud API.
// If mtls is provided, the connection uses mTLS with the given client certificate.
// Otherwise, it uses standard TLS (for local dev).
func newDataCapGRPCConn(addr string, mtls *datacapMTLSConfig) (*grpc.ClientConn, error) {
	var tlsCfg tls.Config

	if mtls != nil {
		if mtls.CACertPath == "" || mtls.ClientCertPath == "" || mtls.ClientKeyPath == "" {
			return nil, fmt.Errorf("datacap mTLS requires all three paths: --datacap-ca-cert, --datacap-client-cert, --datacap-client-key")
		}

		// Load client certificate
		clientCert, err := tls.LoadX509KeyPair(mtls.ClientCertPath, mtls.ClientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("loading datacap client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{clientCert}

		// Load CA cert to verify the server's self-signed certificate.
		caCert, err := os.ReadFile(mtls.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("reading datacap CA cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse datacap CA cert")
		}
		tlsCfg.RootCAs = caPool
	}

	creds := credentials.NewTLS(&tlsCfg)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dialing datacap gRPC server %s: %w", addr, err)
	}
	return conn, nil
}
