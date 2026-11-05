package node

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

const stateFile = "node.json"

// Ports are chosen from the dynamic range, away from the well-known ports a
// censor or scanner tries first.
const (
	minPort = 20000
	maxPort = 65000
)

// state is what a node keeps across restarts: the device ID lantern-cloud
// knows it by, and the port its inbound listens on. The port is random so
// nodes can't be found by scanning one port, and kept so a node's router
// mapping and firewall rule stay valid.
type state struct {
	DeviceID string `json:"device_id"`
	Port     uint16 `json:"port"`
}

// loadState reads the node's state from dir, creating it on first run.
func loadState(dir string) (state, error) {
	path := filepath.Join(dir, stateFile)
	b, err := os.ReadFile(path)
	if err == nil {
		var s state
		if err := json.Unmarshal(b, &s); err != nil {
			return state{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if s.DeviceID == "" || s.Port == 0 {
			return state{}, fmt.Errorf("%s has no device ID or port", path)
		}
		return s, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return state{}, fmt.Errorf("read %s: %w", path, err)
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return state{}, fmt.Errorf("generate device ID: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return state{}, err
	}
	s := state{DeviceID: hex.EncodeToString(id), Port: port}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return state{}, fmt.Errorf("create %s: %w", dir, err)
	}
	b, err = json.MarshalIndent(s, "", "  ")
	if err != nil {
		return state{}, fmt.Errorf("marshal state: %w", err)
	}
	// Written whole, synced and renamed into place, so a crash or power loss
	// can't leave a partial file that loses the device ID.
	tmp, err := os.CreateTemp(dir, stateFile+".*")
	if err != nil {
		return state{}, fmt.Errorf("create state file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return state{}, fmt.Errorf("write state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return state{}, fmt.Errorf("write state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return state{}, fmt.Errorf("write state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return state{}, fmt.Errorf("save %s: %w", path, err)
	}
	// Syncing the directory makes the rename durable too. Not every platform
	// can sync a directory, so a failure only costs that.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return s, nil
}

// freePort picks a random port in [minPort, maxPort) that nothing on the host
// is listening on over TCP.
func freePort() (uint16, error) {
	for range 20 {
		n, err := rand.Int(rand.Reader, big.NewInt(maxPort-minPort))
		if err != nil {
			return 0, fmt.Errorf("pick port: %w", err)
		}
		port := uint16(minPort + n.Int64())
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(int(port))))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, errors.New("no free port found")
}
