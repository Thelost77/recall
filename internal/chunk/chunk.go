package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Thelost77/recall/internal/model"
	"golang.org/x/text/unicode/norm"
)

const (
	TargetWords  = 250
	OverlapWords = 40
)

var nonSpace = regexp.MustCompile(`\S+`)

func Entries(entries []model.Entry) []model.Chunk {
	var chunks []model.Chunk
	for _, entry := range entries {
		parts := split(entry.Text, TargetWords, OverlapWords)
		for index, text := range parts {
			hash := sha256.Sum256([]byte(text))
			chunks = append(chunks, model.Chunk{
				Key: stableChunkKey(entry.Key, index), SessionKey: entry.SessionKey,
				EntryKey: entry.Key, EntryNativeID: entry.NativeID,
				EntryParentID: entry.ParentID, Kind: entry.Kind,
				Part: index, Role: entry.Role, Timestamp: entry.Timestamp, Text: text,
				TextHash: hex.EncodeToString(hash[:]), Grams: Grams(text),
			})
		}
	}
	return chunks
}

func split(text string, target, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	matches := nonSpace.FindAllStringIndex(text, -1)
	if len(matches) <= target {
		return []string{text}
	}
	if overlap >= target {
		overlap = target / 5
	}
	step := target - overlap
	parts := make([]string, 0, (len(matches)+step-1)/step)
	for start := 0; start < len(matches); start += step {
		end := start + target
		if end > len(matches) {
			end = len(matches)
		}
		left := matches[start][0]
		right := matches[end-1][1]
		parts = append(parts, strings.TrimSpace(text[left:right]))
		if end == len(matches) {
			break
		}
	}
	return parts
}

func Normalize(text string) string {
	decomposed := norm.NFKD.String(strings.ToLower(text))
	var builder strings.Builder
	builder.Grow(len(decomposed))
	space := true
	for _, r := range decomposed {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			space = false
		default:
			if !space {
				builder.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

func Grams(text string) string {
	set := GramSet(text)
	values := make([]string, 0, len(set))
	for gram := range set {
		values = append(values, gram)
	}
	sort.Strings(values)
	return strings.Join(values, " ")
}

func GramSet(text string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, token := range strings.Fields(Normalize(text)) {
		runes := []rune(token)
		width := 3
		if len(runes) < width {
			width = len(runes)
		}
		if width == 0 {
			continue
		}
		for index := 0; index+width <= len(runes); index++ {
			set[string(runes[index:index+width])] = struct{}{}
		}
	}
	return set
}

func Similarity(query, candidate map[string]struct{}) float64 {
	if len(query) == 0 || len(candidate) == 0 {
		return 0
	}
	intersection := 0
	for gram := range query {
		if _, ok := candidate[gram]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(len(query))
}

func stableChunkKey(entryKey string, part int) string {
	hash := sha256.Sum256([]byte(entryKey + "\x00" + strconv.Itoa(part)))
	return hex.EncodeToString(hash[:])
}
