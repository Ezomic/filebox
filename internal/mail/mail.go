package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnauthorized = errors.New("mail service refused the token")
	ErrRejected     = errors.New("mail service rejected the message")
	ErrRateLimited  = errors.New("mail service is rate limiting us")
	ErrUnavailable  = errors.New("mail service is unavailable")
)

const sendTimeout = 10 * time.Second

// Sender hands one plain text message to a mail service. An implementation must not retry:
// the service sends again every time it is asked.
type Sender interface {
	Send(ctx context.Context, to, subject, text string) error
}

// Zero sends through the outbound API of the owner's zero app, so filebox never holds SMTP credentials.
type Zero struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewZero(baseURL, token string) *Zero {
	return &Zero{
		endpoint: strings.TrimRight(baseURL, "/") + "/api/outbound",
		token:    token,
		client: &http.Client{
			Timeout:       sendTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (z *Zero) SetTimeout(timeout time.Duration) {
	z.client.Timeout = timeout
}

type outbound struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

// Errors never carry the token, the address or the body, so they are safe to log.
func (z *Zero) Send(ctx context.Context, to, subject, text string) error {
	payload, err := json.Marshal(outbound{To: to, Subject: subject, Text: text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: bad request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+z.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := z.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: no answer", ErrUnavailable)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusUnprocessableEntity:
		return ErrRejected
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}

	return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
}

// Config lives in mail.json in the data directory, written by the owner, never in the repo.
type Config struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	PortalURL string `json:"portal_url"`
}

var ErrBadConfig = errors.New("mail config is incomplete")

// Valid insists on https for everything except loopback, so the token and the reset links never travel in clear text.
func (c Config) Valid() error {
	if c.Token == "" || !secureURL(c.URL) || !secureURL(c.PortalURL) {
		return ErrBadConfig
	}

	return nil
}

func secureURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()

		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}

	return false
}

// Message is what Fake records.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Fake records messages instead of sending them.
type Fake struct {
	Err error

	mu   sync.Mutex
	sent []Message
}

func (f *Fake) Send(_ context.Context, to, subject, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.sent = append(f.sent, Message{To: to, Subject: subject, Text: text})

	return nil
}

func (f *Fake) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Message(nil), f.sent...)
}
