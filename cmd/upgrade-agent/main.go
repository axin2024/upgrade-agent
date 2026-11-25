package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/records"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
	"github.com/axin2024/upgrade-agent/internal/web"
)

var version = "dev"
var revision = "unreleased"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "version", "--version":
		fmt.Printf("upgrade-agent %s (%s)\n", version, revision)
		return nil
	case "capture":
		return capture(args[1:])
	case "request":
		return request(ctx, args[1:])
	case "migrate":
		opts := flag.NewFlagSet("migrate", flag.ContinueOnError)
		steps := opts.Int("steps", 0, "apply only this many forward migrations")
		if err := opts.Parse(args[1:]); err != nil {
			return err
		}
		if *steps < 0 || opts.NArg() != 0 {
			return errors.New("invalid forward migration arguments")
		}
		return records.Migrate(os.Getenv("UPGRADE_MYSQL_DSN"), *steps)
	case "provision", "revoke":
		return access(ctx, args[0], args[1:])
	case "serve":
		return serve(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Println(`Upgrade Agent
  capture    Capture local allow-listed PHP and Composer files without writing to the repository.
  migrate    Apply the embedded MySQL migrations.
  provision  Create a workspace-scoped operator credential.
  revoke     Revoke an operator's workspace credentials.
  serve      Start the workspace API and browser workbench.
  request    Call a workspace API endpoint with a scoped credential.
  version    Print the installed version.

Use <command> -h for command flags.`)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func capture(args []string) error {
	opts := flag.NewFlagSet("capture", flag.ContinueOnError)
	directory := opts.String("root", ".", "repository directory")
	allow := opts.String("allow", "composer.json,composer.lock,app,tests", "comma-separated repository paths")
	commit := opts.String("ref", "", "optional revision identifier recorded with the files")
	expected := opts.String("expected", "", "expected prior baseline when replacing a workspace snapshot")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return errors.New("unexpected capture argument")
	}
	current, err := snapshot.Capture(*directory, *commit, strings.Split(*allow, ","))
	if err != nil {
		return err
	}
	return printJSON(records.BaselineInput{Expected: *expected, Snapshot: current})
}

func access(ctx context.Context, action string, args []string) error {
	opts := flag.NewFlagSet(action, flag.ContinueOnError)
	tenant := opts.String("tenant", "", "tenant identifier")
	workspace := opts.String("workspace", "", "repository workspace identifier")
	actor := opts.String("actor", "", "operator identifier")
	role := opts.String("role", "maintainer", "admin, maintainer, reviewer, or verifier")
	ttl := opts.Duration("ttl", 24*time.Hour, "credential validity duration")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return errors.New("unexpected access argument")
	}
	store, err := records.Open(ctx, os.Getenv("UPGRADE_MYSQL_DSN"))
	if err != nil {
		return errors.New("cannot connect to workspace MySQL")
	}
	defer store.Close()
	if action == "revoke" {
		count, err := store.Revoke(ctx, *tenant, *workspace, *actor)
		if err != nil {
			return err
		}
		return printJSON(map[string]int64{"revoked": count})
	}
	grant, err := store.Provision(ctx, *tenant, *workspace, *actor, *role, *ttl)
	if err != nil {
		return err
	}
	return printJSON(grant)
}

func serve(ctx context.Context, args []string) error {
	opts := flag.NewFlagSet("serve", flag.ContinueOnError)
	address := opts.String("listen", "127.0.0.1:8101", "HTTP listener address")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return errors.New("unexpected serve argument")
	}
	store, err := records.Open(ctx, os.Getenv("UPGRADE_MYSQL_DSN"))
	if err != nil {
		return errors.New("cannot connect to workspace MySQL")
	}
	defer store.Close()
	if err := store.Ready(ctx); err != nil {
		return errors.New("workspace MySQL migrations are required")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	app := web.App{Store: store, Logger: logger}
	if binary, image := os.Getenv("UPGRADE_DOCKER"), os.Getenv("UPGRADE_VALIDATION_IMAGE"); binary != "" || image != "" {
		if !filepath.IsAbs(binary) {
			return errors.New("UPGRADE_DOCKER must be an absolute local executable path")
		}
		backend, err := validation.NewDocker(binary, image, 45*time.Second)
		if err != nil {
			return err
		}
		app.Validation = backend
	}
	if endpoint := os.Getenv("UPGRADE_MODEL_URL"); endpoint != "" {
		app.Model, err = model.New(endpoint, os.Getenv("UPGRADE_MODEL_NAME"), os.Getenv("UPGRADE_MODEL_TOKEN"), 60*time.Second)
		if err != nil {
			return err
		}
	}
	server := &http.Server{
		Addr: *address, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 4 * time.Minute, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	logger.Info("workspace listener starting", "address", *address, "version", version)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func request(ctx context.Context, args []string) error {
	opts := flag.NewFlagSet("request", flag.ContinueOnError)
	base := opts.String("base", "http://127.0.0.1:8101", "HTTPS or loopback HTTP workspace URL")
	key := opts.String("id", "", "idempotency key for POST requests")
	input := opts.String("input", "", "JSON request file or - for stdin")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 2 {
		return errors.New("request [flags] GET|POST path")
	}
	method, path := opts.Arg(0), opts.Arg(1)
	if method != "GET" && method != "POST" || method == "POST" && (*input == "" || *key == "") {
		return errors.New("GET and POST are supported; POST requires -input and -id")
	}
	root, err := url.Parse(*base)
	if err != nil || root.Host == "" || root.User != nil || root.RawQuery != "" || root.Fragment != "" ||
		root.Path != "" && root.Path != "/" {
		return errors.New("invalid workspace base URL")
	}
	address, _ := netip.ParseAddr(root.Hostname())
	if root.Scheme != "https" && !(root.Scheme == "http" && address.IsLoopback()) {
		return errors.New("workspace URL requires HTTPS or a literal loopback HTTP address")
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" || relative.Path == "" ||
		strings.HasPrefix(relative.Path, "/") || strings.Contains(relative.Path, "..") {
		return errors.New("invalid relative workspace API path")
	}
	root.Path, root.RawQuery = "/api/v1/"+relative.Path, relative.RawQuery
	var payload []byte
	if method == "POST" {
		payload, err = inputJSON(*input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, root.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	secret := os.Getenv("UPGRADE_WORKSPACE_TOKEN")
	if len(secret) != 64 || strings.ContainsAny(secret, "\r\n\x00") {
		return errors.New("UPGRADE_WORKSPACE_TOKEN is required")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", *key)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 4 * time.Minute, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("HTTP %d %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	_, err = io.Copy(os.Stdout, response.Body)
	return err
}

func inputJSON(path string) ([]byte, error) {
	var input io.Reader = os.Stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		input = file
	}
	const limit = 32 << 20
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit || !json.Valid(data) {
		return nil, errors.New("input must be valid JSON up to 32 MiB")
	}
	return data, nil
}
