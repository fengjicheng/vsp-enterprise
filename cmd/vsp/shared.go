package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/oisee/vibing-steampunk/internal/config"
	"github.com/oisee/vibing-steampunk/internal/daemon"
	"github.com/spf13/cobra"
)

// Shared backend mode (see internal/daemon)
var (
	sharedMode        bool
	sharedDaemonMode  bool
	sharedIdleTimeout time.Duration
)

func init() {
	rootCmd.Flags().BoolVar(&sharedMode, "shared", false, "Share one backend (SAP session, JCo sidecar, SSO login) between all MCP clients using the same configuration")
	rootCmd.Flags().DurationVar(&sharedIdleTimeout, "shared-idle-timeout", 10*time.Minute, "Stop the shared backend after this long without connected clients (0 = never)")
	rootCmd.Flags().BoolVar(&sharedDaemonMode, "shared-daemon", false, "Internal: run as the shared backend daemon")
	_ = rootCmd.Flags().MarkHidden("shared-daemon")

	rootCmd.AddCommand(daemonCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonStopCmd.Flags().Bool("force", false, "Kill daemons that do not shut down gracefully")
}

// sharedKey identifies the daemon for the effective configuration. Clients with
// identical binary and configuration share a daemon; anything else gets its own.
func sharedKey(cfg *config.GlobalConfig) (string, error) {
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("hashing configuration: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	var exeStamp string
	if st, err := os.Stat(exe); err == nil {
		exeStamp = strconv.FormatInt(st.Size(), 10) + "/" + strconv.FormatInt(st.ModTime().UnixNano(), 10)
	}
	return daemon.Key(
		[]byte(Version), []byte(Commit), []byte(exe), []byte(exeStamp), cfgJSON,
		[]byte(os.Getenv("SNC_LIB")), []byte(os.Getenv("JAVA_HOME")),
	), nil
}

// runSharedShim bridges stdio to the shared daemon, spawning it if needed.
// It returns daemon.ErrSpawnFailed (wrapped) if the caller should serve in-process.
func runSharedShim(key string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: %v", daemon.ErrSpawnFailed, err)
	}
	wd, _ := os.Getwd()
	args := append(append([]string{}, os.Args[1:]...), "--shared-daemon")

	return daemon.RunShim(context.Background(), daemon.ShimOptions{
		Key:     key,
		Exe:     exe,
		Args:    args,
		Dir:     wd,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Verbose: cfg.Verbose,
	})
}

func systemIDs(cfg *config.GlobalConfig) []string {
	ids := make([]string, 0, len(cfg.Systems))
	for id := range cfg.Systems {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage shared backend daemons (--shared mode)",
	Long: `Manage background daemons started by "vsp --shared".

With --shared, every MCP client session connects to one background daemon per
configuration instead of running its own SAP connection. The daemon stops by
itself after --shared-idle-timeout without clients.`,
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "List running shared daemons",
	RunE: func(_ *cobra.Command, _ []string) error {
		running, err := daemon.List()
		if err != nil {
			return err
		}
		if len(running) == 0 {
			fmt.Println("No shared daemons running.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "KEY\tPID\tVERSION\tCLIENTS\tREADY\tUPTIME\tSYSTEMS")
		for _, r := range running {
			if r.Err != nil {
				_, _ = fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\tnot responding: %v\n", r.Key, r.Err)
				continue
			}
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%t\t%s\t%v\n", r.Key, r.PID, r.Version, r.Clients, r.Ready,
				time.Since(r.Started).Round(time.Second), r.Systems)
		}
		return w.Flush()
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop all shared daemons",
	Long: `Stop all shared daemons of the current user.

Connected MCP clients keep running; their next request starts a fresh daemon
(including a new SSO login if configured). Stop daemons before replacing the
vsp binary on Windows, where a running executable cannot be overwritten.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		force, _ := cmd.Flags().GetBool("force")
		running, err := daemon.List()
		if err != nil {
			return err
		}
		if len(running) == 0 {
			fmt.Println("No shared daemons running.")
			return nil
		}
		var failed int
		for _, r := range running {
			if err := daemon.Stop(r, force); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				failed++
				continue
			}
			fmt.Printf("Stopped daemon %s\n", r.Key)
		}
		if failed > 0 {
			return fmt.Errorf("%d daemon(s) could not be stopped", failed)
		}
		return nil
	},
}
