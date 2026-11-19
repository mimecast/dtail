package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/journal"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

type readSlotAcquirer interface {
	AcquireReadSlot(context.Context, omode.Mode, string) (release func(), acquired bool)
	TryAcquireReadSlot(omode.Mode, string) (release func(), acquired bool)
}

type readLimiter func(context.Context, string) (release func(), acquired bool)

type readerFactoryOptions struct {
	slots          readSlotAcquirer
	target         *fs.ValidatedReadTarget
	path           string
	globID         string
	serverMessages chan<- string
	maxLineLength  int
	logger         logging.Logger
}

type readerFactory func(readerFactoryOptions) (fs.FileReader, readLimiter, error)

var readerFactories = map[omode.Mode]readerFactory{
	omode.CatClient:  makeReaderFactory(omode.CatClient, false),
	omode.GrepClient: makeReaderFactory(omode.GrepClient, false),
	omode.TailClient: makeReaderFactory(omode.TailClient, true),
}

func makeReaderFactory(mode omode.Mode, seekEOF bool) readerFactory {
	return func(options readerFactoryOptions) (fs.FileReader, readLimiter, error) {
		if options.slots == nil {
			return nil, nil, fmt.Errorf("reader factory for %s (%d): missing read slot acquirer", mode, mode)
		}

		reader, err := makeReader(options, mode, seekEOF)
		if err != nil {
			return nil, nil, err
		}
		limiter := func(ctx context.Context, path string) (func(), bool) {
			return options.slots.AcquireReadSlot(ctx, mode, path)
		}
		return reader, limiter, nil
	}
}

func makeReader(options readerFactoryOptions, mode omode.Mode, seekEOF bool) (fs.FileReader, error) {
	if options.target != nil && options.target.Kind == fs.JournalKind {
		return journal.NewReader(journalArgs(options.path), options.path,
			mode == omode.TailClient, options.serverMessages)
	}

	return fs.NewReadFile(fs.ReadOptions{
		Mode:           mode,
		Target:         options.target,
		FilePath:       options.path,
		GlobID:         options.globID,
		ServerMessages: options.serverMessages,
		SeekEOF:        seekEOF,
		MaxLineLength:  options.maxLineLength,
		Logger:         options.logger,
	})
}

func readerFactoryFor(mode omode.Mode) readerFactory {
	factory, ok := readerFactories[mode]
	if !ok {
		return readerFactories[omode.TailClient]
	}
	return factory
}

func journalArgs(spec string) []string {
	source := strings.TrimPrefix(spec, fs.JournalSpecPrefix)
	if source == "" {
		return nil
	}
	return []string{"-u", source}
}
