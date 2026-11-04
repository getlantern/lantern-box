package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/getlantern/lantern-box/node"
)

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().Bool("register", false, "Get this node's config from lantern-cloud, by registering it as a Lantern node")
	serveCmd.Flags().String("api-url", "https://df.iantem.io/api/v1", "lantern-cloud API base URL")
	serveCmd.Flags().String("state-dir", "", "Directory keeping the node's device ID and port (default: lantern-box under the user's config directory)")
	serveCmd.Flags().Uint16("port", 0, "Port for the node's inbound, in place of the random one chosen on first run; forward it if the node is behind a router")
	serveCmd.Flags().String("external-ip", "", "Public IP clients reach the node at (default: the address lantern-cloud sees the node's requests come from)")
	serveCmd.Flags().String("user-id", "", "Your Lantern user ID, which can enroll this node")
	serveCmd.Flags().Int("max-peers", 0, "How many censored users the Unbounded volunteer serves at once (default: lantern-box's)")
	serveCmd.Flags().String("bandwidth-limit", "", "Cap on the Unbounded volunteer's traffic in each direction, such as 10mbit or 2MB/s (default: none)")
	serveCmd.Flags().Duration("rotation-interval", node.DefaultRotationInterval, "How often to register again, for fresh credentials and settings")
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve as a Lantern node, with config from lantern-cloud",
	RunE: func(cmd *cobra.Command, args []string) error {
		register, err := cmd.Flags().GetBool("register")
		if err != nil {
			return fmt.Errorf("get register flag: %w", err)
		}
		if !register {
			return errors.New("serve needs --register: a node gets its config from lantern-cloud (use run for a config file)")
		}
		f := cmd.Flags()
		cfg := node.Config{Version: version, FeatureOverrides: os.Getenv("LANTERN_FEATURE_OVERRIDES")}
		for _, err := range []error{
			getString(f.GetString, "api-url", &cfg.APIURL),
			getString(f.GetString, "state-dir", &cfg.StateDir),
			getString(f.GetString, "external-ip", &cfg.ExternalIP),
			getString(f.GetString, "user-id", &cfg.UserID),
			getString(f.GetString, "bandwidth-limit", &cfg.BandwidthLimit),
		} {
			if err != nil {
				return err
			}
		}
		if cfg.Port, err = f.GetUint16("port"); err != nil {
			return fmt.Errorf("get port flag: %w", err)
		}
		if cfg.MaxPeers, err = f.GetInt("max-peers"); err != nil {
			return fmt.Errorf("get max-peers flag: %w", err)
		}
		if cfg.RotationInterval, err = f.GetDuration("rotation-interval"); err != nil {
			return fmt.Errorf("get rotation-interval flag: %w", err)
		}
		if cfg.StateDir == "" {
			dir, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("find a state directory, or pass --state-dir: %w", err)
			}
			cfg.StateDir = filepath.Join(dir, "lantern-box")
		}
		ctx, stop := signal.NotifyContext(globalCtx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return node.Run(ctx, cfg)
	},
}

func getString(get func(string) (string, error), name string, dst *string) error {
	v, err := get(name)
	if err != nil {
		return fmt.Errorf("get %s flag: %w", name, err)
	}
	*dst = v
	return nil
}
