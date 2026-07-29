package main

import "os"

func main() {
	os.Exit(newApplication().Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
