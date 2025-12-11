package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"
)

const socket = "/var/run/wireguard/wg0.sock"

func init() {
	rootCmd.AddCommand(wgPeersCmd)

	addPeerCmd.Flags().String("public-key", "", "Public key of the peer to add")
	addPeerCmd.Flags().StringSlice("allowed-ips", []string{"0.0.0.0/0"}, "Allowed IPs for the peer")
	addPeerCmd.Flags().String("socket", socket, "WireGuard socket path")
	addPeerCmd.MarkFlagRequired("public-key")

	removePeerCmd.Flags().String("public-key", "", "Public key of the peer to remove")
	removePeerCmd.Flags().String("socket", socket, "WireGuard socket path")
	removePeerCmd.MarkFlagRequired("public-key")

	listPeersCmd.Flags().String("socket", socket, "WireGuard socket path")

	wgPeersCmd.AddCommand(addPeerCmd)
	wgPeersCmd.AddCommand(removePeerCmd)
	wgPeersCmd.AddCommand(listPeersCmd)
}

var wgPeersCmd = &cobra.Command{
	Use:   "wg-peers",
	Short: "Manage WireGuard peers",
	Long:  `Add or remove WireGuard peers`,
}

var addPeerCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a WireGuard peer",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("public-key")
		allowedIPs, _ := cmd.Flags().GetStringSlice("allowed-ips")
		sock, _ := cmd.Flags().GetString("socket")
		err := addPeer(key, allowedIPs, sock)
		if err != nil {
			return fmt.Errorf("adding peer: %w", err)
		}
		fmt.Println("Peer added")
		return nil
	},
}

func addPeer(publicKey string, allowedIPs []string, wgIfc string) error {
	publicKeyHex, err := keyToHex(publicKey)
	if err != nil {
		return err
	}

	req := "public_key=" + publicKeyHex + "\n"
	for _, ip := range allowedIPs {
		_, _, err := net.ParseCIDR(ip)
		if err != nil {
			return fmt.Errorf("parsing allowed IP %s: %w", ip, err)
		}
		req += "allowed_ip=" + ip + "\n"
	}
	_, err = sendReq("set=1\n"+req, wgIfc)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	return nil
}

var removePeerCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove a WireGuard peer",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("public-key")
		sock, _ := cmd.Flags().GetString("socket")
		err := removePeer(key, sock)
		if err != nil {
			return fmt.Errorf("removing peer: %w", err)
		}
		fmt.Println("Peer removed")
		return nil
	},
}

func removePeer(publicKey string, wgIfc string) error {
	publicKeyHex, err := keyToHex(publicKey)
	if err != nil {
		return err
	}

	req := "set=1\npublic_key=" + publicKeyHex + "\nremove=true\n"
	_, err = sendReq(req, wgIfc)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	return nil
}

var listPeersCmd = &cobra.Command{
	Use:   "list",
	Short: "List WireGuard peers",
	Long:  `List all WireGuard peers`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sock, _ := cmd.Flags().GetString("socket")
		res, err := sendReq("get=1\n\n", sock)
		if err != nil {
			return fmt.Errorf("retrieving peers: %w", err)
		}
		idx := strings.Index(res, "public_key")
		if idx == -1 {
			fmt.Println("No peers found.")
			return nil
		}
		printPeers(res[idx:])
		return nil
	},
}

func printPeers(s string) {
	var peers []string
	s = strings.TrimSpace(s)
	for {
		idx := strings.Index(s[1:], "public_key")
		if idx == -1 {
			peers = append(peers, s)
			break
		}
		idx++ // adjust for the offset
		peers = append(peers, s[:idx])
		s = s[idx:]
	}
	fmt.Println("--------------------------------")
	fmt.Println(strings.Join(peers, "\n--------------------------------\n"))
	fmt.Println()
}

func keyToHex(publicKey string) (string, error) {
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		return "", fmt.Errorf("decoding public key: %w", err)
	}
	return hex.EncodeToString(publicKeyBytes), nil
}

func sendReq(req, wgSock string) (string, error) {
	conn, err := net.Dial("unix", wgSock)
	if err != nil {
		return "", fmt.Errorf("dialing socket: %w", err)
	}
	defer conn.Close()

	switch {
	case req[len(req)-1] != '\n': // has no trailing newline
		req += "\n\n"
	case req[len(req)-2:] != "\n\n": // has one trailing newline
		req += "\n"
	}
	_, err = conn.Write([]byte(req))
	if err != nil {
		return "", fmt.Errorf("writing to socket: %w", err)
	}

	r := bufio.NewReader(conn)
	out := make([]byte, 0, 1024)
	for {
		b, err := r.ReadBytes('\n')
		if err != nil {
			return "", fmt.Errorf("reading from socket: %w", err)
		}
		if bytes.Equal(b, []byte{'\n'}) {
			return string(out), nil
		}
		out = append(out, b...)
	}
}
