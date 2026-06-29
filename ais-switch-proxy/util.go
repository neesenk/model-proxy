package main

import (
	"os"
	"os/exec"
	"runtime"
)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func envOrEmpty(k string) string { return os.Getenv(k) }

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func runtimeOS() string { return runtime.GOOS }

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	return c.Start()
}
