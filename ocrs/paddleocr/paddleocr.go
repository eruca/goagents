package paddleocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/ocrs"
	"github.com/eruca/goagents/ocrs/retrypolicy"
)

type Client struct {
	name       string
	apiURL     string
	tokenPool  *tokenPool
	httpClient *http.Client
	retryMW    ocrs.Middleware[[]byte, ocrs.OCRResult]
}

func NewClient(name, apiURL, token string, timeout time.Duration, opts ...retrypolicy.Option) *Client {
	return NewClientWithTokens(name, apiURL, []string{token}, timeout, opts...)
}

func NewClientWithTokens(name, apiURL string, tokens []string, timeout time.Duration, opts ...retrypolicy.Option) *Client {
	return &Client{
		name:      name,
		apiURL:    apiURL,
		tokenPool: newTokenPool(tokens, 1000),
		httpClient: &http.Client{
			Timeout: timeout,
		},
		retryMW: retrypolicy.NewMiddleware[[]byte, ocrs.OCRResult](IsRetryable, opts...),
	}
}

func (c *Client) Family() string { return "paddleocr" }
func (c *Client) Name() string   { return c.name }

func (c *Client) Handle(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
	if len(data) == 0 {
		return ocrs.OCRResult{}, fmt.Errorf("empty data")
	}

	payload := map[string]any{
		"markdownIgnoreLabels": []string{
			"header",
			"header_image",
			"footer",
			"footer_image",
			"number",
			"footnote",
			"aside_text",
		},
		"file":               base64.StdEncoding.EncodeToString(data),
		"fileType":           0,
		"useLayoutDetection": true,
		"promptLabel":        "ocr",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ocrs.OCRResult{}, fmt.Errorf("marshal request failed: %w", err)
	}

	retryHandler := c.retryMW.Wrap(
		ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(callCtx context.Context, _ []byte) (ocrs.OCRResult, error) {
			return c.recognizeOnce(callCtx, body)
		}),
	)
	return retryHandler.Handle(ctx, nil)
}

func (c *Client) recognizeOnce(ctx context.Context, body []byte) (ocrs.OCRResult, error) {
	var out ocrs.OCRResult
	token, err := c.tokenPool.acquire()
	if err != nil {
		return out, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewBuffer(body))
	if err != nil {
		return out, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, fmt.Errorf("read response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := string(rawBody)
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusForbidden {
			c.tokenPool.markExhausted(token)
		}
		return out, &statusError{statusCode: resp.StatusCode, body: msg}
	}

	var envelope struct {
		ErrorCode int    `json:"errorCode"`
		ErrorMsg  string `json:"errorMsg"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return out, fmt.Errorf("decode response failed: %w", err)
	}
	if envelope.ErrorCode != 0 {
		return out, fmt.Errorf("api error code=%d msg=%s", envelope.ErrorCode, envelope.ErrorMsg)
	}

	out.Provider = c.name
	out.Raw = rawBody
	return out, nil
}

type statusError struct {
	statusCode int
	body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.statusCode, e.body)
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAllTokensExhaustedToday) {
		return false
	}

	var stErr *statusError
	if errors.As(err, &stErr) {
		switch stErr.statusCode {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}

	return false
}

var ErrAllTokensExhaustedToday = errors.New("all tokens exhausted for today")

type tokenPool struct {
	mu         sync.Mutex
	tokens     []string
	next       int
	dailyLimit int
	day        string
	used       map[string]int
}

func newTokenPool(tokens []string, dailyLimit int) *tokenPool {
	cleaned := make([]string, 0, len(tokens))
	for _, t := range tokens {
		tt := strings.TrimSpace(t)
		if tt == "" {
			continue
		}
		cleaned = append(cleaned, tt)
	}
	if dailyLimit <= 0 {
		dailyLimit = 1000
	}
	return &tokenPool{
		tokens:     cleaned,
		dailyLimit: dailyLimit,
		day:        dayKey(),
		used:       make(map[string]int, len(cleaned)),
	}
}

func (p *tokenPool) acquire() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.tokens) == 0 {
		return "", ErrAllTokensExhaustedToday
	}

	p.resetIfNewDayLocked()
	for i := 0; i < len(p.tokens); i++ {
		idx := (p.next + i) % len(p.tokens)
		tk := p.tokens[idx]
		if p.used[tk] >= p.dailyLimit {
			continue
		}
		p.used[tk]++
		p.next = (idx + 1) % len(p.tokens)
		return tk, nil
	}

	return "", ErrAllTokensExhaustedToday
}

func (p *tokenPool) markExhausted(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetIfNewDayLocked()
	p.used[token] = p.dailyLimit
}

func (p *tokenPool) resetIfNewDayLocked() {
	cur := dayKey()
	if cur == p.day {
		return
	}
	p.day = cur
	p.used = make(map[string]int, len(p.tokens))
}

func dayKey() string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Now().Format("2006-01-02")
	}
	return time.Now().In(loc).Format("2006-01-02")
}
