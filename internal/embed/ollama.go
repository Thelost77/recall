package embed

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const preprocessingVersion = "chunks-v2"

// EmbeddingGemma prompt formats for search queries and untitled documents.
const (
	queryPrefix    = "task: search result | query: "
	documentPrefix = "title: none | text: "
)

type Provider interface {
	Fingerprint() string
	EmbedQuery(context.Context, string) ([]float32, error)
	EmbedDocuments(context.Context, []string) ([][]float32, error)
}

type Client struct {
	baseURL *url.URL
	model   string
	http    *http.Client
}

type embedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
}

type embedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	Error      string      `json:"error"`
}

func New(rawURL, model string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse embedding URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("embedding URL must use http or https")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("embedding URL %q is not loopback; remote session upload is disabled", rawURL)
	}
	if model == "" {
		return nil, fmt.Errorf("embedding model is empty")
	}
	return &Client{
		baseURL: parsed, model: model,
		http: &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

func (c *Client) Fingerprint() string {
	return "ollama:" + c.model + ":" + preprocessingVersion
}

func (c *Client) Model() string { return c.model }

func (c *Client) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := c.embed(ctx, []string{queryPrefix + query})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (c *Client) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	prefixed := make([]string, len(texts))
	for index, text := range texts {
		prefixed[index] = documentPrefix + text
	}
	return c.embed(ctx, prefixed)
}

func (c *Client) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts, Truncate: true})
	if err != nil {
		return nil, err
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact Ollama at %s: %w", c.baseURL, err)
	}
	defer response.Body.Close()
	var payload embedResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Ollama response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		if payload.Error != "" {
			return nil, fmt.Errorf("Ollama returned %s: %s", response.Status, payload.Error)
		}
		return nil, fmt.Errorf("Ollama returned %s", response.Status)
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("Ollama embedding failed: %s", payload.Error)
	}
	if len(payload.Embeddings) != len(texts) {
		return nil, fmt.Errorf("Ollama returned %d vectors for %d texts", len(payload.Embeddings), len(texts))
	}

	vectors := make([][]float32, len(payload.Embeddings))
	dimensions := 0
	for index, source := range payload.Embeddings {
		if dimensions == 0 {
			dimensions = len(source)
		}
		if len(source) == 0 || len(source) != dimensions {
			return nil, fmt.Errorf("Ollama returned inconsistent vector dimensions")
		}
		vector := make([]float32, len(source))
		var magnitude float64
		for position, value := range source {
			vector[position] = float32(value)
			magnitude += value * value
		}
		if magnitude == 0 {
			return nil, fmt.Errorf("Ollama returned a zero vector")
		}
		scale := float32(1 / math.Sqrt(magnitude))
		for position := range vector {
			vector[position] *= scale
		}
		vectors[index] = vector
	}
	return vectors, nil
}

func Encode(vector []float32) []byte {
	buffer := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(buffer[index*4:], math.Float32bits(value))
	}
	return buffer
}

func Decode(buffer []byte) ([]float32, error) {
	if len(buffer) == 0 || len(buffer)%4 != 0 {
		return nil, fmt.Errorf("invalid float32 vector length %d", len(buffer))
	}
	vector := make([]float32, len(buffer)/4)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(buffer[index*4:]))
	}
	return vector, nil
}

func Dot(left, right []float32) (float64, error) {
	if len(left) != len(right) {
		return 0, fmt.Errorf("vector dimensions differ: %d and %d", len(left), len(right))
	}
	var score float64
	for index := range left {
		score += float64(left[index] * right[index])
	}
	return score, nil
}
