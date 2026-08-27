package chunk

import (
	"strings"
	"testing"

	"github.com/Thelost77/recall/internal/model"
)

func TestNormalizeFoldsCaseWhitespaceAndDiacritics(t *testing.T) {
	got := Normalize("  Generowanie\nKODÓW  QR! ")
	if got != "generowanie kodow qr" {
		t.Fatalf("Normalize() = %q", got)
	}
}

func TestTypoSharesMostQueryGrams(t *testing.T) {
	query := GramSet("generate qr codes empty state")
	candidate := GramSet("generate printable QR codes from the empty state")
	typo := GramSet("genrate qr cdoes emtpy state")
	if score := Similarity(typo, candidate); score < 0.40 {
		t.Fatalf("typo similarity = %.3f, want >= 0.40", score)
	}
	if score := Similarity(query, candidate); score <= Similarity(GramSet("unrelated invoices"), candidate) {
		t.Fatalf("relevant similarity = %.3f", score)
	}
}

func TestEntriesSplitLongTextDeterministically(t *testing.T) {
	words := make([]string, 600)
	for index := range words {
		words[index] = "word"
	}
	entry := model.Entry{Key: "entry", SessionKey: "session", NativeID: "native", Text: strings.Join(words, " "), Role: "user"}
	first := Entries([]model.Entry{entry})
	second := Entries([]model.Entry{entry})
	if len(first) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(first))
	}
	for index := range first {
		if first[index].Key != second[index].Key || first[index].TextHash != second[index].TextHash {
			t.Fatalf("chunk %d is not deterministic", index)
		}
	}
}
