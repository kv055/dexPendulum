package kraken

import (
	"commons/apierror"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/krakenfx/api-go/v2/pkg/derivatives"
	krakenapi "github.com/krakenfx/api-go/v2/pkg/kraken"
)

// FuturesOptions configures the official Kraken Futures REST client.
type FuturesOptions struct {
	BaseURL string
	Timeout time.Duration
	Client  *http.Client
}

// FuturesClient is a small context-aware adapter around Kraken's official
// derivatives REST SDK. It retains SDK signing/encoding while exposing the
// generic endpoints used by hedge input collection.
type FuturesClient struct {
	rest   *derivatives.REST
	key    string
	secret string
	nonce  *krakenapi.EpochCounter
	client *http.Client
}

func NewFutures(apiKey, apiSecret string, opts FuturesOptions) *FuturesClient {
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://futures.kraken.com"
	}
	client := opts.Client
	if client == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &FuturesClient{
		rest:   &derivatives.REST{BaseURL: baseURL, PublicKey: apiKey, PrivateKey: apiSecret},
		key:    apiKey,
		secret: apiSecret,
		nonce:  krakenapi.NewEpochCounter(),
		client: client,
	}
}

type contextREST struct {
	rest   *derivatives.REST
	ctx    context.Context
	client *http.Client
	nonce  func() string
}

func (r contextREST) NewRequest(opts derivatives.RequestOptions) (*krakenapi.Request, error) {
	clone := *r.rest
	clone.Executor = func(req *http.Request) (*http.Response, error) {
		return r.client.Do(req.WithContext(r.ctx))
	}
	opts.Nonce = r.nonce
	return clone.NewRequest(opts)
}

// Request calls an official Kraken Futures endpoint and decodes its result.
func (c *FuturesClient) Request(ctx context.Context, method, path string, auth bool, query, body any, out any) error {
	resp, err := derivatives.Call[json.RawMessage](contextREST{rest: c.rest, ctx: ctx, client: c.client, nonce: c.nonce.Get}, derivatives.RequestOptions{
		Auth: auth, Method: method, Path: path, Query: query, Body: body,
	})
	// Check the status before the decode error: a rejected request carries
	// Kraken's reason (e.g. insufficientAvailableFunds) or an HTML error page.
	if resp != nil && resp.Http != nil && resp.Http.Response != nil {
		if status := resp.Http.StatusCode; status < 200 || status >= 300 {
			return fmt.Errorf("kraken futures %s %s: %w", method, path, apierror.FromBody(status, resp.Http.Body, "Kraken Futures"))
		}
	}
	if err != nil {
		return err
	}
	if resp.Http == nil || resp.Http.Response == nil {
		return fmt.Errorf("kraken futures: missing HTTP response")
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("kraken futures decode %s: %w", path, err)
		}
	}
	return nil
}
