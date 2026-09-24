// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// The variables design 011 names for reaching a control plane. Environment
// reads them; New never does.
const (
	URLEnv       = "CELLA_URL"
	TokenEnv     = "CELLA_TOKEN"
	TokenFileEnv = "CELLA_TOKEN_FILE"
)

// DefaultTokenPath is where a driver projects a sandbox's own token
// (design 045), so a workload inside a sandbox authenticates with nothing
// configured.
const DefaultTokenPath = "/run/cella/token"

// TokenSource yields the bearer of one request. It is asked once per request
// with that request's context, so a source that refreshes a token, or reads
// one that is rewritten before it expires, hands each request the current
// one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc adapts a function to a TokenSource, which is what a caller with
// its own issuer passes.
type TokenFunc func(ctx context.Context) (string, error)

// Token calls the function.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// StaticToken is a source that yields one fixed bearer.
func StaticToken(token string) TokenSource { return staticToken(token) }

type staticToken string

// Token is the fixed bearer.
func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// TokenFile is a source that reads the bearer from a file on every request,
// surrounding space trimmed. It is read per request and never once: the
// controller rewrites a projected token before it expires, and a followed
// stream outlives one token. A file that cannot be read or holds nothing is a
// *NoBearer.
func TokenFile(path string) TokenSource { return tokenFile(path) }

type tokenFile string

// Token reads the file.
func (f tokenFile) Token(context.Context) (string, error) {
	data, err := os.ReadFile(string(f))
	if err != nil {
		return "", &NoBearer{Path: string(f), Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &NoBearer{Path: string(f)}
	}
	return token, nil
}

// NoBearer reports a token file that yields no bearer: it could not be read,
// or it holds nothing. It is a type of its own because a caller that was
// given no credential made a configuration mistake, which is not a server
// that refused.
type NoBearer struct {
	// Path is the file that was read.
	Path string
	// Err is why it could not be read, nil where it was read and was empty.
	Err error
}

// Error names the file and why it yields no bearer.
func (e *NoBearer) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("no bearer: %v", e.Err)
	}
	return fmt.Sprintf("no bearer: %s holds none", e.Path)
}

// Unwrap is the read's own failure.
func (e *NoBearer) Unwrap() error { return e.Err }

// Environment is design 011's reading of the environment as a Config: the
// address from CELLA_URL, and the bearer from CELLA_TOKEN, else from the file
// CELLA_TOKEN_FILE names, else from the file a driver projects into every
// sandbox. It reads through getenv, os.Getenv when nil, so a command whose
// flags override a variable passes a getenv that answers the flag.
//
// It is a call and never a default of New: code inside a sandbox writes
// client.New(client.Environment(os.Getenv)), and a library caller's
// configuration is exactly what it wrote.
func Environment(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Config{URL: getenv(URLEnv)}
	switch token, file := getenv(TokenEnv), getenv(TokenFileEnv); {
	case token != "":
		cfg.Token = StaticToken(token)
	case file != "":
		cfg.Token = TokenFile(file)
	default:
		cfg.Token = TokenFile(DefaultTokenPath)
	}
	return cfg
}
