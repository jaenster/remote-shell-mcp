package launcher

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	BaseURL    string
	Token      string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	HTTPClient *http.Client
}

func (p *Proxy) Run(ctx context.Context) error {
	if p.HTTPClient == nil {
		p.HTTPClient = &http.Client{Timeout: 0}
	}
	sseURL := strings.TrimRight(p.BaseURL, "/") + "/sse"
	req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("open sse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return errors.New("daemon rejected auth (HTTP 401) — token mismatch or missing")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("sse status %d", resp.StatusCode)
	}

	endpointCh := make(chan string, 1)
	errCh := make(chan error, 2)
	var once sync.Once

	go func() {
		errCh <- p.readSSE(resp.Body, endpointCh, &once)
	}()

	var postURL string
	select {
	case postURL = <-endpointCh:
	case err := <-errCh:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("timed out waiting for sse endpoint event")
	case <-ctx.Done():
		return ctx.Err()
	}

	postURL = resolveURL(p.BaseURL, postURL)

	go func() {
		errCh <- p.readStdin(ctx, postURL)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// readSSE returns errors when the SSE stream ends unexpectedly — silent EOF
// after the connection was successfully established means the daemon went
// away, and the parent client deserves to see it as a transport failure
// rather than a clean exit.
func (p *Proxy) readSSE(body io.Reader, endpointCh chan<- string, once *sync.Once) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	var event, data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			event.Reset()
			return
		}
		evType := event.String()
		payload := data.String()
		event.Reset()
		data.Reset()
		switch evType {
		case "endpoint":
			once.Do(func() { endpointCh <- strings.TrimSpace(payload) })
		case "message", "":
			payload = strings.TrimSuffix(payload, "\n")
			fmt.Fprintln(p.Stdout, payload)
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			flush()
			if err == io.EOF {
				return errors.New("sse stream ended (daemon crashed or restarted)")
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(payload)
			continue
		}
	}
}

func (p *Proxy) readStdin(ctx context.Context, postURL string) error {
	// MCP responses can be arbitrarily large (file reads, resource contents,
	// big tools/list payloads). Use a Reader with ReadBytes('\n'), no limit.
	r := bufio.NewReaderSize(p.Stdin, 64*1024)

	// POSTs are dispatched in parallel goroutines so a slow daemon handler
	// doesn't block subsequent requests. Order doesn't matter on the POST
	// channel — JSON-RPC ids correlate responses received via SSE.
	var wg sync.WaitGroup
	const maxInFlight = 128
	sem := make(chan struct{}, maxInFlight)
	var fatalOnce sync.Once
	var fatalErr atomic.Pointer[error]
	fatalCh := make(chan struct{})
	setFatal := func(err error) {
		fatalOnce.Do(func() {
			e := err
			fatalErr.Store(&e)
			close(fatalCh) // wake the select in the main loop
		})
	}

	// Stdin is a blocking syscall; run it on its own goroutine so the main
	// loop can observe fatal/ctx signals immediately.
	lineCh := make(chan []byte, 16)
	readDoneCh := make(chan error, 1)
	go func() {
		defer close(lineCh)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				buf := append([]byte(nil), line...)
				select {
				case lineCh <- buf:
				case <-ctx.Done():
					readDoneCh <- ctx.Err()
					return
				case <-fatalCh:
					readDoneCh <- nil
					return
				}
			}
			if err != nil {
				readDoneCh <- err
				return
			}
		}
	}()

	dispatch := func(payload []byte) {
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-fatalCh:
			wg.Done()
			return
		case <-ctx.Done():
			wg.Done()
			return
		}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if perr := p.postOne(ctx, postURL, payload); perr != nil {
				setFatal(perr)
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			if fe := fatalErr.Load(); fe != nil {
				return *fe
			}
			return ctx.Err()
		case <-fatalCh:
			wg.Wait()
			return *fatalErr.Load()
		case err := <-readDoneCh:
			wg.Wait()
			if fe := fatalErr.Load(); fe != nil {
				return *fe
			}
			if err == nil || err == io.EOF {
				return nil
			}
			return err
		case payload, ok := <-lineCh:
			if !ok {
				continue // channel closed; readDoneCh will fire next
			}
			dispatch(payload)
		}
	}
}

func (p *Proxy) postOne(ctx context.Context, postURL string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", postURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	// 401 means the token rotated (daemon restarted) — propagate as fatal so
	// the outer reconnect loop re-reads daemon.token and re-attaches. Any
	// other 4xx/5xx is also fatal: we can't recover the JSON-RPC reply for
	// this request and the parent client will time out waiting for it; better
	// to reset the bridge.
	if resp.StatusCode == 401 {
		return errors.New("daemon returned 401 (token rotated or invalid)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	return nil
}

func resolveURL(base, endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	u, err := url.Parse(base)
	if err != nil {
		return endpoint
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.ResolveReference(ref).String()
}
