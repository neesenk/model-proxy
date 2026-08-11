package cli

import (
	"encoding/json"
	"fmt"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/daemonctl"
	displaypkg "model-proxy/provider"
	"os"
)

// cmdSchedule queries the running daemon's /debug/schedule endpoint and prints
// which provider each model is currently scheduled to (first-choice + ordered
// list + sticky state). The daemon (`model-proxy serve`) must be running.
func CmdSchedule(args []string, cfg *configdomain.Config) {
	resp, err := daemonctl.Client.Get("http://" + cfg.Listen + "/debug/schedule")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n",
			displaypkg.Red("✗"), cfg.Listen, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "%s daemon returned HTTP %d: %s\n", displaypkg.Red("✗"), resp.StatusCode, displaypkg.Truncate(string(body), 200))
		os.Exit(1)
	}
	var st StatusSchedule
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(os.Stderr, "%s parse schedule response: %v\n", displaypkg.Red("✗"), err)
		os.Exit(1)
	}
	if len(st.Models) == 0 {
		fmt.Println("(no routes)")
		return
	}
	fmt.Print(RenderScheduleRoutes(st.Models, ""))
}
