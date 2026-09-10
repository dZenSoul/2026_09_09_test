// Command stacktest performs black-box acceptance checks against an already
// running Documents service. It talks only to the public HTTP API, so it also
// verifies the container port, migrations, PostgreSQL and blob storage.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxResponseBytes = 2 << 20

type config struct {
	baseURL        string
	adminToken     string
	readyTimeout   time.Duration
	requestTimeout time.Duration
	mode           string
	loadProfile    string
	loadDuration   time.Duration
	workers        int
	maxOPS         int
	maxErrorRate   float64
	maxP95         time.Duration
}

type runner struct {
	cfg    config
	client *http.Client
	out    io.Writer
	tokens map[string]string
	docs   []ownedDocument
	names  []ownedName
	mu     sync.Mutex
}

type ownedDocument struct {
	id    string
	token string
}

type ownedName struct {
	name  string
	token string
}

type listItem struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	File   bool     `json:"file"`
	Public bool     `json:"public"`
	Grant  []string `json:"grant"`
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "stacktest:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &runner{
		cfg: cfg, client: newHTTPClient(cfg), out: os.Stdout,
		tokens: make(map[string]string),
	}
	if err := r.run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "stacktest: FAILED:", err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("stacktest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := config{}
	fs.StringVar(&cfg.baseURL, "base-url", envOr("STACKTEST_BASE_URL", "http://127.0.0.1:8080"), "service base URL")
	fs.StringVar(&cfg.adminToken, "admin-token", os.Getenv("ADMIN_TOKEN"), "registration token (prefer ADMIN_TOKEN env)")
	fs.DurationVar(&cfg.readyTimeout, "ready-timeout", 60*time.Second, "maximum readiness wait")
	fs.DurationVar(&cfg.requestTimeout, "request-timeout", 10*time.Second, "timeout for one HTTP request")
	fs.StringVar(&cfg.mode, "mode", "smoke", "test mode: smoke or load")
	fs.StringVar(&cfg.loadProfile, "load-profile", "read", "load profile: read or mixed")
	fs.DurationVar(&cfg.loadDuration, "load-duration", 30*time.Second, "load generation duration")
	fs.IntVar(&cfg.workers, "workers", 16, "number of concurrent load workers")
	fs.IntVar(&cfg.maxOPS, "max-ops", 0, "maximum operations per second; 0 means unlimited")
	fs.Float64Var(&cfg.maxErrorRate, "max-error-rate", 0.01, "maximum accepted error ratio from 0 to 1")
	fs.DurationVar(&cfg.maxP95, "max-p95", 2*time.Second, "maximum accepted p95 latency; 0 disables the check")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.baseURL = strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	parsed, err := url.Parse(cfg.baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return config{}, errors.New("-base-url must be an absolute http(s) URL without query or fragment")
	}
	if cfg.adminToken == "" {
		return config{}, errors.New("ADMIN_TOKEN or -admin-token is required")
	}
	if cfg.readyTimeout <= 0 || cfg.requestTimeout <= 0 {
		return config{}, errors.New("timeouts must be positive")
	}
	if cfg.mode != "smoke" && cfg.mode != "load" {
		return config{}, errors.New("-mode must be smoke or load")
	}
	if cfg.loadProfile != "read" && cfg.loadProfile != "mixed" {
		return config{}, errors.New("-load-profile must be read or mixed")
	}
	if cfg.loadDuration <= 0 || cfg.workers <= 0 || cfg.workers > 1000 || cfg.maxOPS < 0 || cfg.maxOPS > 1_000_000 {
		return config{}, errors.New("load duration and workers must be positive, workers at most 1000, max-ops between 0 and 1000000")
	}
	if cfg.maxErrorRate < 0 || cfg.maxErrorRate > 1 || cfg.maxP95 < 0 {
		return config{}, errors.New("-max-error-rate must be between 0 and 1 and -max-p95 must be non-negative")
	}
	return cfg, nil
}

func newHTTPClient(cfg config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	idleConnections := max(100, cfg.workers*2)
	transport.MaxIdleConns = idleConnections
	transport.MaxIdleConnsPerHost = idleConnections
	return &http.Client{Timeout: cfg.requestTimeout, Transport: transport}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (r *runner) run(ctx context.Context) (runErr error) {
	started := time.Now()
	fmt.Fprintf(r.out, "stacktest: target %s\n", r.cfg.baseURL)
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := r.cleanup(cleanupCtx); err != nil {
			fmt.Fprintf(r.out, "WARN cleanup: %v\n", err)
			if runErr == nil {
				runErr = fmt.Errorf("cleanup: %w", err)
			}
		}
	}()

	if err := r.step("readiness", func() error { return r.waitReady(ctx) }); err != nil {
		return err
	}
	if err := r.step("runtime endpoints", func() error { return r.checkRuntime(ctx) }); err != nil {
		return err
	}
	if r.cfg.mode == "load" {
		if err := r.runLoad(ctx); err != nil {
			return err
		}
		if err := r.cleanup(ctx); err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		cleaned = true
		fmt.Fprintf(r.out, "stacktest: LOAD PASS (%s)\n", time.Since(started).Round(time.Millisecond))
		return nil
	}

	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("generate test identity: %w", err)
	}
	logins := map[string]string{
		"owner": "stowner" + suffix, "reader": "streader" + suffix, "stranger": "ststranger" + suffix,
	}
	password := "Stacktest1!"
	if err := r.step("registration and authentication", func() error {
		for _, role := range []string{"owner", "reader", "stranger"} {
			if err := r.register(ctx, logins[role], password); err != nil {
				return fmt.Errorf("register %s: %w", role, err)
			}
			token, err := r.authenticate(ctx, logins[role], password)
			if err != nil {
				return fmt.Errorf("authenticate %s: %w", role, err)
			}
			r.tokens[role] = token
		}
		return nil
	}); err != nil {
		return err
	}

	jsonName := "stacktest-" + suffix + ".json"
	fileName := "stacktest-" + suffix + ".bin"
	wantJSON := json.RawMessage(`{"source":"stacktest","ok":true}`)
	wantFile := []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff, 'o', 'k'}
	r.names = append(r.names, ownedName{jsonName, r.tokens["owner"]}, ownedName{fileName, r.tokens["owner"]})
	if err := r.step("JSON and file upload", func() error {
		if err := r.upload(ctx, map[string]any{
			"name": jsonName, "file": false, "public": false,
			"token": r.tokens["owner"], "grant": []string{logins["reader"]},
		}, wantJSON, nil); err != nil {
			return fmt.Errorf("upload JSON: %w", err)
		}
		if err := r.upload(ctx, map[string]any{
			"name": fileName, "file": true, "public": true,
			"mime": "application/octet-stream", "token": r.tokens["owner"], "grant": []string{},
		}, nil, wantFile); err != nil {
			return fmt.Errorf("upload file: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	var jsonID, fileID string
	if err := r.step("listing and access matrix", func() error {
		ownerDocs, err := r.list(ctx, r.tokens["owner"], "")
		if err != nil {
			return err
		}
		for _, item := range ownerDocs {
			switch item.Name {
			case jsonName:
				jsonID = item.ID
			case fileName:
				fileID = item.ID
			}
		}
		if jsonID == "" || fileID == "" {
			return fmt.Errorf("uploaded documents missing from owner list")
		}
		r.docs = append(r.docs, ownedDocument{jsonID, r.tokens["owner"]}, ownedDocument{fileID, r.tokens["owner"]})

		readerDocs, err := r.list(ctx, r.tokens["reader"], logins["owner"])
		if err != nil {
			return fmt.Errorf("list documents as granted reader: %w", err)
		}
		if !containsIDs(readerDocs, jsonID, fileID) {
			return errors.New("granted reader does not see both documents")
		}
		strangerDocs, err := r.list(ctx, r.tokens["stranger"], logins["owner"])
		if err != nil {
			return fmt.Errorf("list documents as ungranted user: %w", err)
		}
		if len(strangerDocs) != 1 || strangerDocs[0].ID != fileID {
			return errors.New("public access list mismatch")
		}
		status, _, _, err := r.request(ctx, http.MethodGet, "/api/docs?login="+url.QueryEscape(logins["owner"]), nil, "")
		if err != nil {
			return err
		}
		return expectStatus(status, http.StatusUnauthorized, nil)
	}); err != nil {
		return err
	}

	if err := r.step("content, HEAD and authorization", func() error {
		status, _, body, err := r.request(ctx, http.MethodGet, documentPath(jsonID, r.tokens["reader"]), nil, "")
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusOK, body); err != nil {
			return err
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || !jsonEqual(envelope.Data, wantJSON) {
			return fmt.Errorf("JSON response mismatch")
		}
		status, _, body, err = r.request(ctx, http.MethodGet, documentPath(jsonID, r.tokens["stranger"]), nil, "")
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusForbidden, body); err != nil {
			return err
		}

		status, header, body, err := r.request(ctx, http.MethodGet, documentPath(fileID, r.tokens["stranger"]), nil, "")
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusOK, body); err != nil {
			return err
		}
		if !bytes.Equal(body, wantFile) || header.Get("Content-Type") != "application/octet-stream" {
			return fmt.Errorf("downloaded file content or MIME mismatch")
		}
		status, header, body, err = r.request(ctx, http.MethodHead, documentPath(fileID, r.tokens["stranger"]), nil, "")
		if err != nil {
			return err
		}
		if status != http.StatusOK || len(body) != 0 || header.Get("Content-Length") != strconv.Itoa(len(wantFile)) {
			return fmt.Errorf("HEAD response mismatch: status=%d length=%q body-bytes=%d", status, header.Get("Content-Length"), len(body))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("deletion and session revocation", func() error {
		status, _, body, err := r.form(ctx, http.MethodDelete, "/api/docs/"+jsonID, url.Values{"token": {r.tokens["reader"]}})
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusForbidden, body); err != nil {
			return err
		}
		if err := r.deleteDocument(ctx, jsonID, r.tokens["owner"]); err != nil {
			return err
		}
		r.forgetDocument(jsonID)
		status, _, body, err = r.request(ctx, http.MethodGet, documentPath(jsonID, r.tokens["owner"]), nil, "")
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusNotFound, body); err != nil {
			return err
		}

		if err := r.logout(ctx, r.tokens["stranger"]); err != nil {
			return err
		}
		delete(r.tokens, "stranger")
		status, _, body, err = r.request(ctx, http.MethodGet, documentPath(fileID, r.tokens["stranger"]), nil, "")
		if err != nil {
			return err
		}
		return expectStatus(status, http.StatusUnauthorized, body)
	}); err != nil {
		return err
	}

	if err := r.cleanup(ctx); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	cleaned = true
	fmt.Fprintf(r.out, "stacktest: PASS (%s)\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func (r *runner) runLoad(ctx context.Context) error {
	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("generate test identity: %w", err)
	}
	login := "stload" + suffix
	password := "Stacktest1!"
	if err := r.step("load fixture registration", func() error {
		if err := r.register(ctx, login, password); err != nil {
			return err
		}
		token, err := r.authenticate(ctx, login, password)
		if err != nil {
			return err
		}
		r.tokens["load"] = token
		return nil
	}); err != nil {
		return err
	}

	fileName := "stacktest-load-" + suffix + ".bin"
	fileBody := []byte("stacktest-load-payload")
	r.trackName(fileName, r.tokens["load"])
	var fileID string
	if err := r.step("load fixture upload", func() error {
		if err := r.upload(ctx, map[string]any{
			"name": fileName, "file": true, "public": true,
			"mime": "application/octet-stream", "token": r.tokens["load"], "grant": []string{},
		}, nil, fileBody); err != nil {
			return err
		}
		items, err := r.listByName(ctx, r.tokens["load"], fileName)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			return fmt.Errorf("load fixture lookup returned %d documents, want 1", len(items))
		}
		fileID = items[0].ID
		r.trackDocument(fileID, r.tokens["load"])
		return nil
	}); err != nil {
		return err
	}

	fmt.Fprintf(r.out, "LOAD profile=%s workers=%d duration=%s max-ops=%d\n",
		r.cfg.loadProfile, r.cfg.workers, r.cfg.loadDuration, r.cfg.maxOPS)
	stats := newLoadStats(r.cfg.maxP95)
	loadCtx, cancel := context.WithTimeout(ctx, r.cfg.loadDuration)
	defer cancel()

	var permits <-chan time.Time
	var ticker *time.Ticker
	if r.cfg.maxOPS > 0 {
		ticker = time.NewTicker(time.Second / time.Duration(r.cfg.maxOPS))
		defer ticker.Stop()
		permits = ticker.C
	}
	var sequence atomic.Uint64
	var workers sync.WaitGroup
	started := time.Now()
	for worker := 0; worker < r.cfg.workers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if permits != nil {
					select {
					case <-loadCtx.Done():
						return
					case <-permits:
					}
				} else {
					select {
					case <-loadCtx.Done():
						return
					default:
					}
				}

				n := sequence.Add(1)
				operation := r.pickLoadOperation(n)
				opStarted := time.Now()
				err := r.executeLoadOperation(ctx, operation, fileID, fileBody, suffix, n)
				stats.record(operation, time.Since(opStarted), err)
			}
		}()
	}
	workers.Wait()
	elapsed := time.Since(started)
	result := stats.snapshot()
	if result.total == 0 {
		return errors.New("load test completed without operations")
	}

	fmt.Fprintf(r.out, "RESULT operations=%d success=%d errors=%d ops/s=%.1f error-rate=%.2f%% p50<=%s p95<=%s p99<=%s\n",
		result.total, result.success, result.errors, float64(result.total)/elapsed.Seconds(),
		100*result.errorRate(), result.p50, result.p95, result.p99)
	for _, name := range []string{"get", "head", "list", "mutation"} {
		if operation := result.operations[name]; operation.total > 0 {
			fmt.Fprintf(r.out, "  %-8s operations=%d errors=%d\n", name, operation.total, operation.errors)
		}
	}
	for _, example := range result.errorExamples {
		fmt.Fprintf(r.out, "  error: %s\n", example)
	}
	if result.errorRate() > r.cfg.maxErrorRate {
		return fmt.Errorf("load error rate %.4f exceeds limit %.4f", result.errorRate(), r.cfg.maxErrorRate)
	}
	if r.cfg.maxP95 > 0 && result.p95OverLimit {
		return fmt.Errorf("load p95 %s exceeds limit %s", result.p95, r.cfg.maxP95)
	}
	return nil
}

func (r *runner) pickLoadOperation(sequence uint64) string {
	position := sequence % 100
	if r.cfg.loadProfile == "mixed" {
		switch {
		case position < 55:
			return "get"
		case position < 70:
			return "head"
		case position < 85:
			return "list"
		default:
			return "mutation"
		}
	}
	switch {
	case position < 70:
		return "get"
	case position < 85:
		return "head"
	default:
		return "list"
	}
}

func (r *runner) executeLoadOperation(ctx context.Context, operation, fileID string, fileBody []byte, suffix string, sequence uint64) error {
	token := r.tokens["load"]
	switch operation {
	case "get":
		status, header, body, err := r.request(ctx, http.MethodGet, documentPath(fileID, token), nil, "")
		if err != nil {
			return err
		}
		if err := expectStatus(status, http.StatusOK, body); err != nil {
			return err
		}
		if !bytes.Equal(body, fileBody) || header.Get("Content-Type") != "application/octet-stream" {
			return errors.New("GET payload mismatch")
		}
		return nil
	case "head":
		status, header, body, err := r.request(ctx, http.MethodHead, documentPath(fileID, token), nil, "")
		if err != nil {
			return err
		}
		if status != http.StatusOK || len(body) != 0 || header.Get("Content-Length") != strconv.Itoa(len(fileBody)) {
			return fmt.Errorf("HEAD contract mismatch: status=%d", status)
		}
		return nil
	case "list":
		_, err := r.list(ctx, token, "")
		return err
	case "mutation":
		return r.loadMutation(ctx, token, suffix, sequence)
	default:
		return fmt.Errorf("unknown load operation %q", operation)
	}
}

func (r *runner) loadMutation(ctx context.Context, token, suffix string, sequence uint64) error {
	name := fmt.Sprintf("stacktest-load-%s-%d.json", suffix, sequence)
	r.trackName(name, token)
	if err := r.upload(ctx, map[string]any{
		"name": name, "file": false, "public": false, "token": token, "grant": []string{},
	}, json.RawMessage(`{"load":true}`), nil); err != nil {
		return fmt.Errorf("mutation upload: %w", err)
	}
	items, err := r.listByName(ctx, token, name)
	if err != nil {
		return fmt.Errorf("mutation lookup: %w", err)
	}
	if len(items) != 1 {
		return fmt.Errorf("mutation lookup returned %d documents", len(items))
	}
	id := items[0].ID
	r.trackDocument(id, token)
	if err := r.deleteDocument(ctx, id, token); err != nil {
		return fmt.Errorf("mutation delete: %w", err)
	}
	r.forgetDocument(id)
	r.forgetName(name)
	return nil
}

var loadLatencyLimits = []time.Duration{
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	500 * time.Millisecond, time.Second, 1500 * time.Millisecond, 2 * time.Second,
	3 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Hour,
}

type operationResult struct{ total, errors uint64 }

type loadStats struct {
	mu            sync.Mutex
	total         uint64
	success       uint64
	errors        uint64
	buckets       []uint64
	operations    map[string]operationResult
	errorExamples []string
	p95Limit      time.Duration
	overP95Limit  uint64
}

type loadResult struct {
	total, success, errors uint64
	operations             map[string]operationResult
	errorExamples          []string
	p50, p95, p99          string
	p95Duration            time.Duration
	p95OverLimit           bool
}

func newLoadStats(p95Limit time.Duration) *loadStats {
	return &loadStats{
		buckets: make([]uint64, len(loadLatencyLimits)), operations: make(map[string]operationResult),
		p95Limit: p95Limit,
	}
}

func (s *loadStats) record(operation string, latency time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	result := s.operations[operation]
	result.total++
	if err == nil {
		s.success++
	} else {
		s.errors++
		result.errors++
		if len(s.errorExamples) < 5 {
			s.errorExamples = append(s.errorExamples, operation+": "+err.Error())
		}
	}
	s.operations[operation] = result
	if s.p95Limit > 0 && latency > s.p95Limit {
		s.overP95Limit++
	}
	for index, limit := range loadLatencyLimits {
		if latency <= limit {
			s.buckets[index]++
			return
		}
	}
	s.buckets[len(s.buckets)-1]++
}

func (s *loadStats) snapshot() loadResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := loadResult{
		total: s.total, success: s.success, errors: s.errors,
		operations:    make(map[string]operationResult, len(s.operations)),
		errorExamples: append([]string(nil), s.errorExamples...),
	}
	for name, operation := range s.operations {
		result.operations[name] = operation
	}
	result.p50, _ = histogramQuantile(s.buckets, s.total, 0.50)
	result.p95, result.p95Duration = histogramQuantile(s.buckets, s.total, 0.95)
	result.p99, _ = histogramQuantile(s.buckets, s.total, 0.99)
	result.p95OverLimit = s.total > 0 && float64(s.overP95Limit)/float64(s.total) > 0.05
	return result
}

func (r loadResult) errorRate() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.errors) / float64(r.total)
}

func histogramQuantile(buckets []uint64, total uint64, quantile float64) (string, time.Duration) {
	if total == 0 {
		return "n/a", 0
	}
	want := uint64(float64(total)*quantile + 0.999999)
	var cumulative uint64
	for index, count := range buckets {
		cumulative += count
		if cumulative >= want {
			return loadLatencyLimits[index].String(), loadLatencyLimits[index]
		}
	}
	return ">" + loadLatencyLimits[len(loadLatencyLimits)-1].String(), loadLatencyLimits[len(loadLatencyLimits)-1]
}

func (r *runner) step(name string, fn func() error) error {
	started := time.Now()
	if err := fn(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Fprintf(r.out, "PASS %-39s %s\n", name, time.Since(started).Round(time.Millisecond))
	return nil
}

func (r *runner) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.readyTimeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		status, _, body, err := r.request(ctx, http.MethodGet, "/health/ready", nil, "")
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d: %s", status, compactBody(body))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service did not become ready (%s): %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *runner) checkRuntime(ctx context.Context) error {
	status, _, body, err := r.request(ctx, http.MethodGet, "/health/live", nil, "")
	if err != nil {
		return err
	}
	if err := expectStatus(status, http.StatusOK, body); err != nil {
		return fmt.Errorf("/health/live: %w", err)
	}
	var health struct {
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Response.Status != "ok" {
		return errors.New("/health/live response contract mismatch")
	}

	status, _, body, err = r.request(ctx, http.MethodGet, "/metrics", nil, "")
	if err != nil {
		return err
	}
	if err := expectStatus(status, http.StatusOK, body); err != nil {
		return fmt.Errorf("/metrics: %w", err)
	}
	if !bytes.Contains(body, []byte("documents_http_requests_total")) {
		return errors.New("/metrics does not expose Documents request metrics")
	}
	return nil
}

func (r *runner) register(ctx context.Context, login, password string) error {
	status, _, body, err := r.form(ctx, http.MethodPost, "/api/register", url.Values{
		"token": {r.cfg.adminToken}, "login": {login}, "pswd": {password},
	})
	if err != nil {
		return err
	}
	return expectStatus(status, http.StatusOK, body)
}

func (r *runner) authenticate(ctx context.Context, login, password string) (string, error) {
	status, _, body, err := r.form(ctx, http.MethodPost, "/api/auth", url.Values{"login": {login}, "pswd": {password}})
	if err != nil {
		return "", err
	}
	if err := expectStatus(status, http.StatusOK, body); err != nil {
		return "", err
	}
	var envelope struct {
		Response struct {
			Token string `json:"token"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Response.Token == "" {
		return "", errors.New("authentication response has no token")
	}
	return envelope.Response.Token, nil
}

func (r *runner) upload(ctx context.Context, meta map[string]any, jsonPart, filePart []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := writer.WriteField("meta", string(encoded)); err != nil {
		return err
	}
	if jsonPart != nil {
		if err := writer.WriteField("json", string(jsonPart)); err != nil {
			return err
		}
	}
	if filePart != nil {
		part, err := writer.CreateFormFile("file", meta["name"].(string))
		if err != nil {
			return err
		}
		if _, err := part.Write(filePart); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	status, _, response, err := r.request(ctx, http.MethodPost, "/api/docs", &body, writer.FormDataContentType())
	if err != nil {
		return err
	}
	return expectStatus(status, http.StatusOK, response)
}

func (r *runner) list(ctx context.Context, token, owner string) ([]listItem, error) {
	query := url.Values{"token": {token}}
	if owner != "" {
		query.Set("login", owner)
	}
	return r.listQuery(ctx, query)
}

func (r *runner) listByName(ctx context.Context, token, name string) ([]listItem, error) {
	return r.listQuery(ctx, url.Values{"token": {token}, "key": {"name"}, "value": {name}})
}

func (r *runner) listQuery(ctx context.Context, query url.Values) ([]listItem, error) {
	status, _, body, err := r.request(ctx, http.MethodGet, "/api/docs?"+query.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	if err := expectStatus(status, http.StatusOK, body); err != nil {
		return nil, err
	}
	var envelope struct {
		Data struct {
			Docs []listItem `json:"docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode document list: %w", err)
	}
	return envelope.Data.Docs, nil
}

func (r *runner) form(ctx context.Context, method, path string, values url.Values) (int, http.Header, []byte, error) {
	body := values.Encode()
	return r.request(ctx, method, path, strings.NewReader(body), "application/x-www-form-urlencoded")
}

func (r *runner) request(ctx context.Context, method, path string, body io.Reader, contentType string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.baseURL+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := r.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, fmt.Errorf("%s %s: %w", method, safeRoute(req.URL.Path), ctxErr)
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return 0, nil, nil, fmt.Errorf("%s %s request failed: %v", method, safeRoute(req.URL.Path), urlErr.Err)
		}
		return 0, nil, nil, fmt.Errorf("%s %s request failed", method, safeRoute(req.URL.Path))
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read %s response: %w", method, err)
	}
	if len(data) > maxResponseBytes {
		return 0, nil, nil, fmt.Errorf("%s response exceeds %d bytes", method, maxResponseBytes)
	}
	return response.StatusCode, response.Header.Clone(), data, nil
}

func (r *runner) deleteDocument(ctx context.Context, id, token string) error {
	status, _, body, err := r.form(ctx, http.MethodDelete, "/api/docs/"+url.PathEscape(id), url.Values{"token": {token}})
	if err != nil {
		return err
	}
	return expectStatus(status, http.StatusOK, body)
}

func (r *runner) logout(ctx context.Context, token string) error {
	status, _, body, err := r.request(ctx, http.MethodDelete, "/api/auth/"+url.PathEscape(token), nil, "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	return expectStatus(status, http.StatusOK, body)
}

func (r *runner) cleanup(ctx context.Context) error {
	var errs []error
	r.mu.Lock()
	docs := append([]ownedDocument(nil), r.docs...)
	names := append([]ownedName(nil), r.names...)
	r.mu.Unlock()
	known := make(map[string]bool, len(docs))
	for _, doc := range docs {
		known[doc.id] = true
	}
	// Upload responses intentionally do not expose IDs. Discover test documents
	// by their random names so a failure between upload and normal listing does
	// not leave blobs behind.
	for _, target := range names {
		items, err := r.list(ctx, target.token, "")
		if err != nil {
			errs = append(errs, fmt.Errorf("discover %s: %w", target.name, err))
			continue
		}
		for _, item := range items {
			if item.Name == target.name && !known[item.ID] {
				docs = append(docs, ownedDocument{item.ID, target.token})
				known[item.ID] = true
			}
		}
	}
	for _, doc := range docs {
		if err := r.deleteDocument(ctx, doc.id, doc.token); err != nil {
			errs = append(errs, fmt.Errorf("document: %w", err))
		}
	}
	r.mu.Lock()
	r.docs = nil
	r.names = nil
	r.mu.Unlock()
	for role, token := range r.tokens {
		if token == "" {
			continue
		}
		if err := r.logout(ctx, token); err != nil {
			errs = append(errs, fmt.Errorf("%s session: %w", role, err))
		}
	}
	r.tokens = make(map[string]string)
	return errors.Join(errs...)
}

func (r *runner) forgetDocument(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index, doc := range r.docs {
		if doc.id == id {
			r.docs = append(r.docs[:index], r.docs[index+1:]...)
			return
		}
	}
}

func (r *runner) trackDocument(id, token string) {
	r.mu.Lock()
	r.docs = append(r.docs, ownedDocument{id, token})
	r.mu.Unlock()
}

func (r *runner) trackName(name, token string) {
	r.mu.Lock()
	r.names = append(r.names, ownedName{name, token})
	r.mu.Unlock()
}

func (r *runner) forgetName(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index, item := range r.names {
		if item.name == name {
			r.names = append(r.names[:index], r.names[index+1:]...)
			return
		}
	}
}

func documentPath(id, token string) string {
	return "/api/docs/" + url.PathEscape(id) + "?token=" + url.QueryEscape(token)
}

func randomSuffix() (string, error) {
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func containsIDs(items []listItem, ids ...string) bool {
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			return false
		}
	}
	return true
}

func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func safeRoute(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/auth/"):
		return "/api/auth/{token}"
	case strings.HasPrefix(path, "/api/docs/"):
		return "/api/docs/{id}"
	default:
		return path
	}
}

func expectStatus(got, want int, body []byte) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("HTTP status %d, want %d (body: %s)", got, want, compactBody(body))
}

func compactBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 240 {
		text = text[:240] + "..."
	}
	return strconv.Quote(text)
}
