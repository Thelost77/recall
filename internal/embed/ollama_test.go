package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVectorEncodingRoundTrip(t *testing.T) {
	original := []float32{1, -0.5, 0.25}
	decoded, err := Decode(Encode(original))
	if err != nil {
		t.Fatal(err)
	}
	for index := range original {
		if original[index] != decoded[index] {
			t.Fatalf("value %d = %f", index, decoded[index])
		}
	}
}

func TestDotRejectsDifferentDimensions(t *testing.T) {
	if _, err := Dot([]float32{1}, []float32{1, 2}); err == nil {
		t.Fatal("Dot accepted different dimensions")
	}
	score, err := Dot([]float32{1, 0}, []float32{0.5, 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(score-0.5) > 0.0001 {
		t.Fatalf("score = %f", score)
	}
}

func TestNewRejectsRemoteEmbeddingURL(t *testing.T) {
	if _, err := New("https://embedding.example.com", "embeddinggemma-2:270m"); err == nil {
		t.Fatal("New accepted a non-loopback URL")
	}
	if _, err := New("http://127.0.0.1:11434", "embeddinggemma-2:270m"); err != nil {
		t.Fatal(err)
	}
}

func TestNewAcceptsOnlyEmbeddingGemmaModels(t *testing.T) {
	for _, model := range []string{"embeddinggemma-2:270m", "embeddinggemma:300m", "EmbeddingGemma"} {
		if _, err := New("http://127.0.0.1:11434", model); err != nil {
			t.Errorf("New(%q) = %v, want nil", model, err)
		}
	}
	for _, model := range []string{"all-minilm", "nomic-embed-text"} {
		if _, err := New("http://127.0.0.1:11434", model); err == nil {
			t.Errorf("New(%q) accepted a model that is not EmbeddingGemma", model)
		}
	}
}

func TestClientAddsEmbeddingGemmaPrompts(t *testing.T) {
	var inputs [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request embedRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		inputs = append(inputs, request.Input)
		vectors := make([][]float64, len(request.Input))
		for index := range vectors {
			vectors[index] = []float64{1, 0}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: vectors})
	}))
	defer server.Close()
	client, err := New(server.URL, "embeddinggemma-2:270m")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.EmbedQuery(ctx, "migration decision"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedDocuments(ctx, []string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"task: search result | query: migration decision"},
		{"title: none | text: first", "title: none | text: second"},
	}
	if len(inputs) != len(want) {
		t.Fatalf("requests = %q, want %q", inputs, want)
	}
	for request := range want {
		if len(inputs[request]) != len(want[request]) {
			t.Fatalf("requests = %q, want %q", inputs, want)
		}
		for index := range want[request] {
			if inputs[request][index] != want[request][index] {
				t.Fatalf("requests = %q, want %q", inputs, want)
			}
		}
	}
}
