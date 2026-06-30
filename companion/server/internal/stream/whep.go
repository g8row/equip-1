package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// WHEPResponse is the result of forwarding a WHEP offer to mediamtx.
type WHEPResponse struct {
	Status      int
	ContentType string
	Body        []byte
	RetryAfter  string // non-empty → set Retry-After header (stream-not-ready case)
}

// ErrWHEPConnect indicates the WHEP upstream (mediamtx) was unreachable.
var ErrWHEPConnect = errors.New("cannot reach mediamtx WHEP endpoint")

// ForwardOffer POSTs the browser's SDP offer to mediamtx's WHEP endpoint,
// retrying on 404 (no publisher yet) up to 4 attempts with 1.5s between tries.
// If still 404 after retries it returns a 503 "stream not ready" body. Mirrors
// the retry/503 logic in main.py whep_proxy.
func ForwardOffer(ctx context.Context, whepURL string, offer []byte) (*WHEPResponse, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	var last *http.Response
	var lastBody []byte
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, whepURL, bytes.NewReader(offer))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/sdp")

		resp, err := client.Do(req)
		if err != nil {
			if isConnError(err) {
				slog.Error("whep-proxy-connect-error", "error", err)
				return nil, ErrWHEPConnect
			}
			slog.Error("whep-proxy-error", "error", err)
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		last = resp
		lastBody = body
		slog.Info("whep-proxy", "attempt", attempt+1, "status", resp.StatusCode)
		if resp.StatusCode != http.StatusNotFound {
			break
		}
		// 404 = no publisher yet; wait and retry.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}

	if last != nil && last.StatusCode == http.StatusNotFound {
		return &WHEPResponse{
			Status:      http.StatusServiceUnavailable,
			ContentType: "application/json",
			Body:        []byte(`{"error": "stream not ready -- camera may be disconnected"}`),
			RetryAfter:  "3",
		}, nil
	}

	contentType := last.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/sdp"
	}
	return &WHEPResponse{
		Status:      last.StatusCode,
		ContentType: contentType,
		Body:        lastBody,
	}, nil
}

// ForwardPatch forwards a WHEP trickle-ICE PATCH to mediamtx, passing the
// incoming headers through. Mirrors whep_patch_proxy.
func ForwardPatch(ctx context.Context, whepURL string, body []byte, headers http.Header) (*WHEPResponse, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, whepURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return &WHEPResponse{Status: resp.StatusCode, Body: respBody}, nil
}

func isConnError(err error) bool {
	var netErr *net.OpError
	return errors.As(err, &netErr)
}
