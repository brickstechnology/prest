package cmd

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prest/prest/v2/config"
)

// miniship: rest's image carries no shell and no HTTP client, so a container
// runtime's health check runs this binary: `prestd health` asks the server
// already running in the same container for /_health on loopback, and exits
// 0 when it answers 200. /_health touches no database, so neither does this.
// A health check may name the address itself, so the file that runs it says
// which path and port it asks.

var healthCmd = &cobra.Command{
	Use:   "health [url]",
	Short: "Ask the running server for /_health on loopback; exit 0 when it answers 200",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		url := healthURL(configFrom(cmd))
		if len(args) == 1 {
			url = args[0]
		}
		if err := probeHealth(url, 5*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	},
}

// healthURL is the loopback address of the health route this process's
// configuration serves.
func healthURL(cfg *config.Prest) string {
	scheme := "http"
	if cfg.HTTPSMode {
		scheme = "https"
	}
	prefix := strings.TrimSuffix(cfg.ContextPath, "/")
	return scheme + "://127.0.0.1:" + strconv.Itoa(cfg.HTTPPort) + prefix + "/_health"
}

// probeHealth returns nil when url answers 200 within timeout.
func probeHealth(url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Loopback, to the server in this container: the certificate it
			// serves is for the names a browser uses, not for 127.0.0.1.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s answered %d", url, resp.StatusCode)
	}
	return nil
}
