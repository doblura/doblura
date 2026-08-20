// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// hashPassword is `console hash`, which prints a bcrypt hash for the accounts
// Secret.
//
// A subcommand rather than a documented one-liner, because the documented
// one-liner is `htpasswd -bnBC 12 "" pass`, which puts the password in the shell
// history and in every process listing on the machine. This reads it from the
// terminal without echo, and refuses to read from a pipe unless asked, so the
// obvious way to use it is also the safe one.
func hashPassword(args []string) {
	var pw []byte
	var err error

	if len(args) > 0 && args[0] == "--stdin" {
		r := bufio.NewReader(os.Stdin)
		line, e := r.ReadString('\n')
		// io.EOF with data is a password that simply had no trailing newline,
		// which is what `printf '%s' pw |` and most pipes produce. Treating it as
		// an error refused a password it had already read, and said "no password
		// read" while holding one.
		if e == io.EOF && line != "" {
			e = nil
		}
		pw, err = []byte(strings.TrimRight(line, "\r\n")), e
	} else {
		fmt.Fprint(os.Stderr, "password: ")
		pw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	}
	if err != nil || len(pw) == 0 {
		fmt.Fprintln(os.Stderr, "no password read")
		os.Exit(1)
	}
	// Cost 12: about a quarter of a second on current hardware, which is slow
	// enough to matter against a stolen Secret and fast enough that a login does
	// not feel broken.
	h, err := bcrypt.GenerateFromPassword(pw, 12)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(h))
}
