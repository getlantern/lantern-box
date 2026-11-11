package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/getlantern/lantern-box/node"
)

func init() {
	rootCmd.AddCommand(serveCmd)
	defineServeFlags(serveCmd.Flags())
	serveCmd.Flags().String("flags-file", "", "File of more serve flags, separated by whitespace, with # comments; the command line overrides them, and a missing file means none")
}

// defineServeFlags defines the flags serve takes on its command line and in
// its flags file.
func defineServeFlags(fs *pflag.FlagSet) {
	fs.Bool("register", false, "Get this node's config from lantern-cloud, by registering it as a Lantern node")
	fs.String("api-url", "https://df.iantem.io/api/v1", "lantern-cloud API base URL")
	fs.String("state-dir", "", "Directory keeping the node's device ID and port (default: lantern-box under the user's config directory)")
	fs.Uint16("port", 0, "Port for the node's inbound, in place of the random one chosen on first run; forward it if the node is behind a router")
	fs.String("user-id", "", "Your Lantern user ID, sent with the node's requests to lantern-cloud (not needed to run one)")
	fs.Int("max-peers", 0, "How many censored users the Unbounded volunteer serves at once (default: lantern-box's)")
	fs.String("bandwidth-limit", "", "Cap on the Unbounded volunteer's traffic in each direction, such as 10mbit or 2MB/s (default: none)")
	fs.Duration("rotation-interval", node.DefaultRotationInterval, "How often to register again, for fresh credentials and settings")
	fs.Bool("no-port-mapping", false, "Don't ask the router, through UPnP, NAT-PMP or PCP, to forward the node's TCP port")
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve as a Lantern node, with config from lantern-cloud",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := cmd.Flags().GetString("flags-file")
		if err != nil {
			return fmt.Errorf("get flags-file flag: %w", err)
		}
		if path != "" {
			if err := applyFlagsFile(cmd.Flags(), path); err != nil {
				return err
			}
		}
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
		if cfg.SkipPortMapping, err = f.GetBool("no-port-mapping"); err != nil {
			return fmt.Errorf("get no-port-mapping flag: %w", err)
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
