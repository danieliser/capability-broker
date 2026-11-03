package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/danieliser/capability-broker/pkg/config"
)

var secretName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// runSecret: brokerd secret set NAME (value from stdin, never echoed or logged) | brokerd secret list | brokerd secret rm NAME
func runSecret(args []string) int {
	if len(args) == 0 {
		return fail("usage: brokerd secret set <NAME> | list | rm <NAME>")
	}
	cfg := config.FromEnv()
	path := cfg.SecretsPath()
	secrets := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &secrets); err != nil {
			return fail("parse %s: %v", path, err)
		}
	}
	switch args[0] {
	case "list":
		names := make([]string, 0, len(secrets))
		for k := range secrets {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Println(n)
		}
		return 0
	case "set", "rm":
		if len(args) != 2 || !secretName.MatchString(args[1]) {
			return fail("usage: brokerd secret %s <NAME> (letters, digits, underscore)", args[0])
		}
		if args[0] == "rm" {
			delete(secrets, args[1])
		} else {
			if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprintf(os.Stderr, "paste the value for %s, then Enter: ", args[1])
			}
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			value := strings.TrimRight(line, "\r\n")
			if value == "" {
				if err != nil {
					return fail("no value on stdin")
				}
				return fail("empty value")
			}
			secrets[args[1]] = value
		}
		b, _ := json.MarshalIndent(secrets, "", "  ")
		if err := atomicWrite(path, b); err != nil {
			return fail("%v", err)
		}
		_ = os.Chmod(path, 0o600)
		fmt.Printf("%s %s in %s (connectors reload within 2s)\n", args[1], map[string]string{"set": "saved", "rm": "removed"}[args[0]], path)
		return 0
	}
	return fail("usage: brokerd secret set <NAME> | list | rm <NAME>")
}
