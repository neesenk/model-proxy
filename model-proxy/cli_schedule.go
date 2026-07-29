package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
)

// cmdSchedule queries the running daemon's /debug/schedule endpoint and prints
// which provider each model is currently scheduled to (first-choice + ordered
// list + sticky state). The daemon (`model-proxy serve`) must be running.
func cmdSchedule(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	resp, err := daemonHTTPClient.Get("http://" + cfg.Listen + "/debug/schedule")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n",
			cRed("✗"), cfg.Listen, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "%s daemon returned HTTP %d: %s\n", cRed("✗"), resp.StatusCode, truncate(string(body), 200))
		os.Exit(1)
	}
	var st statusSchedule
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(os.Stderr, "%s parse schedule response: %v\n", cRed("✗"), err)
		os.Exit(1)
	}
	if len(st.Models) == 0 {
		fmt.Println("(no routes)")
		return
	}
	fmt.Print(renderScheduleRoutes(st.Models, ""))
}
