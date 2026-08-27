package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Thelost77/recall/internal/chunk"
	"github.com/Thelost77/recall/internal/embed"
	"github.com/Thelost77/recall/internal/model"
	"github.com/Thelost77/recall/internal/store"
)

const embeddingBatchSize = 32

type Options struct {
	Reembed  bool
	LockHeld bool
	Progress func(string)
}

type Result struct {
	Discovered       int
	UpdatedSources   int
	UnchangedSources int
	DeletedSources   int
	ParserErrors     int
	EmbeddedChunks   int
	EmbeddingError   error
}

type Indexer struct {
	Store    *store.Store
	Adapters []model.Adapter
	Embedder embed.Provider
}

func (i *Indexer) Run(ctx context.Context, options Options) (Result, error) {
	if !options.LockHeld {
		lock, err := AcquireLock(i.Store.Path() + ".lock")
		if err != nil {
			return Result{}, err
		}
		defer lock.Close()
	}

	if options.Reembed {
		if err := i.Store.ClearEmbeddings(ctx); err != nil {
			return Result{}, fmt.Errorf("clear embeddings: %w", err)
		}
	}

	var result Result
	var adapterErrors []error
	for _, sourceAdapter := range i.Adapters {
		progress(options, "Discovering "+sourceAdapter.Name()+" sessions...")
		sources, err := sourceAdapter.Discover(ctx)
		if err != nil {
			adapterErrors = append(adapterErrors, fmt.Errorf("%s discovery: %w", sourceAdapter.Name(), err))
			continue
		}
		versions, err := i.Store.SourceVersions(ctx, sourceAdapter.Name())
		if err != nil {
			return result, fmt.Errorf("read %s source state: %w", sourceAdapter.Name(), err)
		}
		present := make(map[string]bool, len(sources))
		result.Discovered += len(sources)
		for _, source := range sources {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			present[source.Key] = true
			if versions[source.Key] == source.Version {
				result.UnchangedSources++
				continue
			}
			parsed, err := sourceAdapter.Parse(ctx, source)
			if err != nil {
				result.ParserErrors++
				if recordErr := i.Store.RecordSourceError(ctx, source, err); recordErr != nil {
					return result, fmt.Errorf("record parser error: %w", recordErr)
				}
				progress(options, fmt.Sprintf("Skipping %s: %v", source.Path, err))
				continue
			}
			chunks := chunk.Entries(parsed.Entries)
			if err := i.Store.UpsertSource(ctx, source, parsed, chunks); err != nil {
				return result, err
			}
			if len(parsed.Warnings) > 0 {
				warning := errors.New(strings.Join(parsed.Warnings, "; "))
				if err := i.Store.RecordSourceError(ctx, source, warning); err != nil {
					return result, fmt.Errorf("record parser warning: %w", err)
				}
				result.ParserErrors++
				progress(options, fmt.Sprintf("Indexed %s with warning: %v", source.Path, warning))
			}
			result.UpdatedSources++
		}
		deleted, err := i.Store.DeleteMissingSources(ctx, sourceAdapter.Name(), present)
		if err != nil {
			return result, fmt.Errorf("delete missing %s sources: %w", sourceAdapter.Name(), err)
		}
		result.DeletedSources += deleted
	}

	if i.Embedder != nil {
		progress(options, "Generating missing embeddings...")
		count, embeddingErr := i.backfill(ctx, options)
		result.EmbeddedChunks = count
		result.EmbeddingError = embeddingErr
	}
	if err := i.Store.MarkIndexed(ctx); err != nil {
		return result, fmt.Errorf("record index completion: %w", err)
	}
	if len(adapterErrors) > 0 {
		return result, errors.Join(adapterErrors...)
	}
	return result, nil
}

func (i *Indexer) backfill(ctx context.Context, options Options) (int, error) {
	fingerprint := i.Embedder.Fingerprint()
	total := 0
	for {
		pending, err := i.Store.PendingChunks(ctx, fingerprint, embeddingBatchSize)
		if err != nil {
			return total, err
		}
		if len(pending) == 0 {
			return total, nil
		}
		texts := make([]string, len(pending))
		ids := make([]int64, len(pending))
		for index, item := range pending {
			texts[index] = item.Text
			ids[index] = item.ID
		}
		vectors, err := i.Embedder.Embed(ctx, texts)
		if err != nil {
			return total, err
		}
		dimensions := len(vectors[0])
		encoded := make([][]byte, len(vectors))
		for index, vector := range vectors {
			if len(vector) != dimensions {
				return total, fmt.Errorf("embedding batch contains inconsistent dimensions")
			}
			encoded[index] = embed.Encode(vector)
		}
		if err := i.Store.UpdateEmbeddings(ctx, fingerprint, dimensions, ids, encoded); err != nil {
			return total, err
		}
		total += len(pending)
		if total%256 == 0 {
			progress(options, fmt.Sprintf("Embedded %d chunks...", total))
		}
	}
}

func progress(options Options, message string) {
	if options.Progress != nil {
		options.Progress(message)
	}
}

