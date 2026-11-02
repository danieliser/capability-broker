package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os/exec"
	"time"
)

// runAuth: brokerd auth <connector> [--force] [--no-browser] | brokerd auth <connector> --sign-out
// Signs the broker in to a remote OAuth server once; every client and agent then uses that sign-in.
func runAuth(args []string) int {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	force := fs.Bool("force", false, "discard the stored token and sign in again")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening it")
	signOut := fs.Bool("sign-out", false, "forget the stored token")
	name := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil || name == "" {
		return fail("usage: brokerd auth <connector> [--force] [--no-browser] [--sign-out]")
	}
	c, _, err := adminClient()
	if err != nil {
		return fail("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	path := "/v1/auth/" + url.PathEscape(name)
	if *signOut {
		if err := c.Do(ctx, "DELETE", path, nil, nil); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%s signed out\n", name)
		return 0
	}
	if *force {
		path += "?force=1"
	}
	var begin struct {
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	if err := c.Do(ctx, "POST", path, nil, &begin); err != nil {
		return fail("%v", err)
	}
	if begin.Status == "signed_in" {
		fmt.Printf("%s is already signed in and connected (use --force to sign in again)\n", name)
		return 0
	}
	fmt.Printf("Sign in to %s:\n  %s\n", name, begin.URL)
	if !*noBrowser {
		_ = exec.Command("open", begin.URL).Start()
	}
	fmt.Println("Waiting for the browser to finish (10 minutes)...")
	for {
		select {
		case <-ctx.Done():
			return fail("%s: sign-in did not finish", name)
		case <-time.After(time.Second):
		}
		var st struct {
			Status    string `json:"status"`
			Connected bool   `json:"connected"`
		}
		if err := c.Do(ctx, "GET", "/v1/auth/"+url.PathEscape(name), nil, &st); err != nil {
			continue
		}
		if st.Connected {
			fmt.Printf("%s signed in and connected; its tools are live in every broker client.\n", name)
			return 0
		}
	}
}
