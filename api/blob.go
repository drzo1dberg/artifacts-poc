package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// blobClient talks to the blobstore. With a real storage account only this file would be replaced.
type blobClient struct {
	baseURL string
	http    *http.Client
}

func newBlobClient(baseURL string) *blobClient {
	return &blobClient{baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}}
}

func (b *blobClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	u := b.baseURL + (&url.URL{Path: path, RawQuery: query.Encode()}).String()
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("blobstore %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// send is do for calls that only care whether they succeeded.
func (b *blobClient) send(ctx context.Context, method, path string, query url.Values, body io.Reader) error {
	resp, err := b.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (b *blobClient) createContainer(ctx context.Context, container string) error {
	return b.send(ctx, http.MethodPut, "/"+container, nil, nil)
}

func (b *blobClient) put(ctx context.Context, container, name, content string) error {
	return b.send(ctx, http.MethodPut, "/"+container+"/"+name, nil, strings.NewReader(content))
}

func (b *blobClient) get(ctx context.Context, container, name string) (string, error) {
	resp, err := b.do(ctx, http.MethodGet, "/"+container+"/"+name, nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

func (b *blobClient) list(ctx context.Context, container, prefix string) ([]string, error) {
	resp, err := b.do(ctx, http.MethodGet, "/"+container, url.Values{"prefix": {prefix}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var names []string
	return names, json.NewDecoder(resp.Body).Decode(&names)
}

func (b *blobClient) deletePrefix(ctx context.Context, container, prefix string) error {
	return b.send(ctx, http.MethodDelete, "/"+container, url.Values{"prefix": {prefix}}, nil)
}
