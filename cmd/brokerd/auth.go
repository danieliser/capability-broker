package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
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
	if err := fs.Parse(args); err != nil {
		return fail("usage: brokerd auth [<connector>|<prefix>] [--force] [--no-browser] [--sign-out]")
	}
	c, _, err := adminClient()
	if err != nil {
		return fail("%v", err)
	}
	// Resolve the name: exact connector, or every connector starting with it ("cloudflare" → cloudflare-api,
	// cloudflare-operator). With no name, list what needs signing in.
	var st struct {
		Connectors map[string]string `json:"connectors"`
	}
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = c.Do(sctx, "GET", "/v1/status", nil, &st)
	scancel()
	if name == "" {
		n := 0
		for conn, state := range st.Connectors {
			if strings.HasPrefix(state, "needs sign-in") {
				fmt.Printf("%-24s needs sign-in\n", conn)
				n++
			}
		}
		if n == 0 {
			fmt.Println("every oauth connector is signed in")
		} else {
			fmt.Println("run: brokerd auth <connector>")
		}
		return 0
	}
	if _, exact := st.Connectors[name]; !exact {
		var matches []string
		for conn := range st.Connectors {
			if strings.HasPrefix(conn, name) {
				matches = append(matches, conn)
			}
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			return fail("no connector named %s", name)
		}
		rc := 0
		for _, m := range matches {
			fmt.Printf("== %s\n", m)
			sub := append([]string{m}, args...)
			if r := runAuth(sub); r != 0 {
				rc = r
			}
		}
		return rc
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
