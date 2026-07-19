package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

const (
	timelineIndexStride        int64 = 256
	timelineInitialWindowLines       = 512
)

type timelineQuery struct {
	Limit     int
	AfterSeq  int64
	HasAfter  bool
	BeforeSeq int64
	HasBefore bool
}

type timelinePage struct {
	Events        []TimelineEvent
	HasMoreBefore bool
	NextBeforeSeq *int64
	HasMoreAfter  bool
}

type timelineLineCheckpoint struct {
	LineNo int64
	Offset int64
}

type timelineLineIndex struct {
	Version         timelineFileVersion
	LineCount       int64
	EndsWithNewline bool
	Checkpoints     []timelineLineCheckpoint
}

type timelinePageKey struct {
	Version timelineFileVersion
	Query   timelineQuery
}

type timelinePageLoadCall struct {
	done chan struct{}
	page timelinePage
	err  error
}

var (
	timelineLineIndexMu    sync.Mutex
	timelineLineIndexes    = map[string]timelineLineIndex{}
	timelineLineIndexLoads = map[string]chan struct{}{}
	timelinePageCacheMu    sync.Mutex
	timelinePageCache      = map[timelinePageKey]timelinePage{}
	timelinePageLoadMu     sync.Mutex
	timelinePageLoads      = map[timelinePageKey]*timelinePageLoadCall{}
	timelinePageReadHook   func(string, int64)
)

func cloneTimelinePage(page timelinePage) timelinePage {
	clone := page
	clone.Events = cloneTimelineEvents(page.Events)
	if page.NextBeforeSeq != nil {
		value := *page.NextBeforeSeq
		clone.NextBeforeSeq = &value
	}
	return clone
}

func timelinePageFor(record *sessionRecord, query timelineQuery) (timelinePage, error) {
	if record == nil || record.SessionFile == "" {
		return timelinePage{}, fmt.Errorf("timeline record is unavailable")
	}
	if query.Limit <= 0 {
		query.Limit = 200
	}
	index, err := ensureTimelineLineIndex(record.SessionFile)
	if err != nil {
		return timelinePage{}, err
	}
	key := timelinePageKey{Version: index.Version, Query: query}
	timelinePageCacheMu.Lock()
	if cached, ok := timelinePageCache[key]; ok {
		timelinePageCacheMu.Unlock()
		return cloneTimelinePage(cached), nil
	}
	timelinePageCacheMu.Unlock()

	timelinePageLoadMu.Lock()
	if call := timelinePageLoads[key]; call != nil {
		done := call.done
		timelinePageLoadMu.Unlock()
		<-done
		return cloneTimelinePage(call.page), call.err
	}
	call := &timelinePageLoadCall{done: make(chan struct{})}
	timelinePageLoads[key] = call
	timelinePageLoadMu.Unlock()

	if timelineParseHook != nil {
		timelineParseHook(record.SessionFile)
	}
	call.page, call.err = readTimelinePage(record, index, query)
	if call.err == nil {
		timelinePageCacheMu.Lock()
		for cachedKey := range timelinePageCache {
			if cachedKey.Version.path == key.Version.path && cachedKey.Version != key.Version {
				delete(timelinePageCache, cachedKey)
			}
		}
		timelinePageCache[key] = cloneTimelinePage(call.page)
		timelinePageCacheMu.Unlock()
	}
	timelinePageLoadMu.Lock()
	delete(timelinePageLoads, key)
	close(call.done)
	timelinePageLoadMu.Unlock()
	return cloneTimelinePage(call.page), call.err
}

func ensureTimelineLineIndex(path string) (timelineLineIndex, error) {
	version, err := timelineVersionForPath(path)
	if err != nil {
		return timelineLineIndex{}, err
	}
	for {
		timelineLineIndexMu.Lock()
		cached, ok := timelineLineIndexes[path]
		if ok && cached.Version == version {
			timelineLineIndexMu.Unlock()
			return cached, nil
		}
		if done := timelineLineIndexLoads[path]; done != nil {
			timelineLineIndexMu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		timelineLineIndexLoads[path] = done
		timelineLineIndexMu.Unlock()

		base := timelineLineIndex{}
		if ok && cached.Version.device == version.device && cached.Version.inode == version.inode && version.size > cached.Version.size {
			base = cached
		}
		next, buildErr := extendTimelineLineIndex(path, version, base)

		timelineLineIndexMu.Lock()
		if buildErr == nil {
			timelineLineIndexes[path] = next
		}
		delete(timelineLineIndexLoads, path)
		close(done)
		timelineLineIndexMu.Unlock()
		return next, buildErr
	}
}

func extendTimelineLineIndex(path string, version timelineFileVersion, base timelineLineIndex) (timelineLineIndex, error) {
	start := int64(0)
	next := timelineLineIndex{Version: version}
	if base.Version.path != "" {
		start = base.Version.size
		next.LineCount = base.LineCount
		next.EndsWithNewline = base.EndsWithNewline
		next.Checkpoints = append([]timelineLineCheckpoint(nil), base.Checkpoints...)
	}
	f, err := os.Open(path)
	if err != nil {
		return timelineLineIndex{}, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return timelineLineIndex{}, err
	}
	buffer := make([]byte, 1024*1024)
	offset := start
	atLineStart := start == 0 || next.EndsWithNewline
	remaining := version.size - start
	for remaining > 0 {
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining
		}
		count, readErr := f.Read(buffer[:readSize])
		for index := 0; index < count; index++ {
			absolute := offset + int64(index)
			if atLineStart {
				next.LineCount++
				if (next.LineCount-1)%timelineIndexStride == 0 {
					next.Checkpoints = append(next.Checkpoints, timelineLineCheckpoint{LineNo: next.LineCount, Offset: absolute})
				}
				atLineStart = false
			}
			if buffer[index] == '\n' {
				atLineStart = true
			}
		}
		offset += int64(count)
		remaining -= int64(count)
		if readErr != nil {
			if readErr != io.EOF {
				return timelineLineIndex{}, readErr
			}
			break
		}
		if count == 0 {
			return timelineLineIndex{}, io.ErrUnexpectedEOF
		}
	}
	next.EndsWithNewline = version.size > 0 && atLineStart
	return next, nil
}

func timelineOffsetForLine(path string, index timelineLineIndex, lineNo int64) (int64, error) {
	if lineNo <= 1 {
		return 0, nil
	}
	if lineNo > index.LineCount {
		return index.Version.size, nil
	}
	checkpointIndex := sort.Search(len(index.Checkpoints), func(i int) bool {
		return index.Checkpoints[i].LineNo > lineNo
	}) - 1
	if checkpointIndex < 0 {
		checkpointIndex = 0
	}
	checkpoint := index.Checkpoints[checkpointIndex]
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(checkpoint.Offset, io.SeekStart); err != nil {
		return 0, err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	offset := checkpoint.Offset
	for current := checkpoint.LineNo; current < lineNo; current++ {
		chunk, readErr := reader.ReadBytes('\n')
		offset += int64(len(chunk))
		if readErr != nil {
			if readErr == io.EOF {
				return index.Version.size, nil
			}
			return 0, readErr
		}
	}
	return offset, nil
}

func readTimelineLineRange(path string, index timelineLineIndex, startLine, endLine int64) ([]byte, error) {
	if startLine < 1 {
		startLine = 1
	}
	if endLine > index.LineCount {
		endLine = index.LineCount
	}
	if startLine > endLine || index.LineCount == 0 {
		return nil, nil
	}
	startOffset, err := timelineOffsetForLine(path, index, startLine)
	if err != nil {
		return nil, err
	}
	endOffset, err := timelineOffsetForLine(path, index, endLine+1)
	if err != nil {
		return nil, err
	}
	if endOffset < startOffset {
		return nil, fmt.Errorf("timeline line offsets are reversed")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	section := io.NewSectionReader(f, startOffset, endOffset-startOffset)
	data, err := io.ReadAll(section)
	if timelinePageReadHook != nil {
		timelinePageReadHook(path, int64(len(data)))
	}
	return data, err
}

func readTimelinePage(record *sessionRecord, index timelineLineIndex, query timelineQuery) (timelinePage, error) {
	if query.Limit <= 0 {
		query.Limit = 200
	}
	if index.LineCount == 0 {
		return timelinePage{Events: []TimelineEvent{}}, nil
	}
	if query.HasAfter {
		return readTimelineAfterPage(record, index, query)
	}
	endLine := index.LineCount
	if query.HasBefore {
		candidate := query.BeforeSeq / 1000
		if candidate < endLine {
			endLine = candidate
		}
	}
	if endLine < 1 {
		return timelinePage{Events: []TimelineEvent{}}, nil
	}
	window := int64(timelineInitialWindowLines)
	if minimum := int64(query.Limit * 2); window < minimum {
		window = minimum
	}
	for {
		startLine := endLine - window + 1
		if startLine < 1 {
			startLine = 1
		}
		data, err := readTimelineLineRange(record.SessionFile, index, startLine, endLine)
		if err != nil {
			return timelinePage{}, err
		}
		events, err := timelineEventsFromRange(record, data, startLine)
		if err != nil {
			return timelinePage{}, err
		}
		if query.HasBefore {
			events = filterEventsBefore(events, query.BeforeSeq)
		}
		if len(events) > query.Limit || startLine == 1 {
			hasMore := len(events) > query.Limit
			if hasMore {
				events = events[len(events)-query.Limit:]
			}
			page := timelinePage{Events: cloneTimelineEvents(events), HasMoreBefore: hasMore}
			if hasMore && len(page.Events) > 0 {
				value := page.Events[0].Seq
				page.NextBeforeSeq = &value
			}
			return page, nil
		}
		window *= 2
	}
}

func readTimelineAfterPage(record *sessionRecord, index timelineLineIndex, query timelineQuery) (timelinePage, error) {
	startLine := query.AfterSeq / 1000
	if startLine < 1 {
		startLine = 1
	}
	if startLine > index.LineCount {
		return timelinePage{Events: []TimelineEvent{}}, nil
	}
	window := int64(timelineInitialWindowLines)
	if minimum := int64(query.Limit * 2); window < minimum {
		window = minimum
	}
	for {
		endLine := startLine + window - 1
		if endLine > index.LineCount {
			endLine = index.LineCount
		}
		data, err := readTimelineLineRange(record.SessionFile, index, startLine, endLine)
		if err != nil {
			return timelinePage{}, err
		}
		events, err := timelineEventsFromRange(record, data, startLine)
		if err != nil {
			return timelinePage{}, err
		}
		events = filterEventsAfter(events, query.AfterSeq)
		if len(events) > query.Limit {
			return timelinePage{Events: cloneTimelineEvents(events[:query.Limit]), HasMoreAfter: true}, nil
		}
		if endLine == index.LineCount {
			return timelinePage{Events: cloneTimelineEvents(events)}, nil
		}
		window *= 2
	}
}

func timelineEventsFromRange(record *sessionRecord, data []byte, firstLine int64) ([]TimelineEvent, error) {
	reader := bytes.NewReader(data)
	if record.Kind == "claude" {
		return claudeTimelineReaderWithContext(reader, firstLine, record.timelineToolNames, record.timelineQueued)
	}
	return codexTimelineReader(reader, firstLine, record.timelineToolNames)
}

func filterEventsBefore(events []TimelineEvent, before int64) []TimelineEvent {
	end := sort.Search(len(events), func(index int) bool { return events[index].Seq >= before })
	return events[:end]
}

func filterEventsAfter(events []TimelineEvent, after int64) []TimelineEvent {
	start := sort.Search(len(events), func(index int) bool { return events[index].Seq > after })
	return events[start:]
}

func resetTimelinePageStateForTest(t interface{ Cleanup(func()) }) {
	timelineLineIndexMu.Lock()
	previousIndexes := timelineLineIndexes
	timelineLineIndexes = map[string]timelineLineIndex{}
	timelineLineIndexMu.Unlock()
	timelinePageCacheMu.Lock()
	previousPages := timelinePageCache
	timelinePageCache = map[timelinePageKey]timelinePage{}
	timelinePageCacheMu.Unlock()
	t.Cleanup(func() {
		timelineLineIndexMu.Lock()
		timelineLineIndexes = previousIndexes
		timelineLineIndexMu.Unlock()
		timelinePageCacheMu.Lock()
		timelinePageCache = previousPages
		timelinePageCacheMu.Unlock()
	})
}
