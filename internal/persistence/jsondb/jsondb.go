// Copyright (C) 2024 The Daguflow/Dagu Authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
package jsondb

import (
	"bufio"
	"sync"

	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/daguflow/dagu/internal/logger"
	"github.com/daguflow/dagu/internal/persistence"
	"github.com/daguflow/dagu/internal/persistence/filecache"
	"github.com/daguflow/dagu/internal/persistence/model"
	"github.com/daguflow/dagu/internal/util"
)

var (
	_ persistence.HistoryStore = (*JSONDB)(nil)
)

const (
	defaultCacheSize = 300
	requestIDLenSafe = 8
	extDat           = ".dat"
	dateTimeFormat   = "20060102.15:04:05.000"
	dateFormat       = "20060102"
)

// JSONDB manages DAG status files in local storage.
type JSONDB struct {
	baseDir           string                          // Base directory for storing files
	writer            *writer                         // Current writer for active status updates
	cache             *filecache.Cache[*model.Status] // Cache for storing parsed status files
	latestStatusToday bool                            // Flag to determine if only today's latest status should be returned
	mu                sync.Mutex                      // Mutex for synchronizing access to shared resources
	logger            logger.Logger                   // Logger for recording events and errors
}

// New creates a new JSONDB instance with default configuration.
func New(baseDir string, logger logger.Logger, latestStatusToday bool) *JSONDB {
	s := &JSONDB{
		baseDir:           baseDir,
		cache:             filecache.New[*model.Status](defaultCacheSize, 3*time.Hour),
		latestStatusToday: latestStatusToday,
		logger:            logger,
	}
	s.cache.StartEviction()
	return s
}

// Update updates the status of a specific DAG execution.
func (s *JSONDB) Update(dagID, reqID string, status *model.Status) error {
	f, err := s.FindByRequestID(dagID, reqID)
	if err != nil {
		return err
	}

	w, err := newWriter(f.File)
	if err != nil {
		return fmt.Errorf("failed to open writer: %w", err)
	}

	defer func() {
		s.cache.Invalidate(f.File)
		_ = w.close()
	}()

	return w.write(status)
}

// Open initializes a new writer for a DAG execution.
func (s *JSONDB) Open(dagID string, t time.Time, requestID string) error {
	if s.writer != nil {
		return persistence.ErrWriterOpen
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filename := craftStatusFile(dagID, requestID, t)

	// Index file is used to index the status files by its filename.
	// Status files are stored in date-wise directories and indexed by the index file
	// Both index and status files are named with the same filename but stored in different directories
	// Filename format: <dagID>.<timestamp>.<requestID>.dat
	//
	// Note that dagID can be different from the actual DAG name because
	// renaming a DAG does not rename the status files.
	// Therefore, index file name should not be renamed once created.
	indexFile := filepath.Join(craftIndexDataDir(s.baseDir, dagID), filename)
	statusFile := filepath.Join(craftStatusDataDir(s.baseDir, t), filename)

	// make directories
	if err := os.MkdirAll(filepath.Dir(indexFile), 0755); err != nil {
		return fmt.Errorf("failed to create index directory: %w", err)
	}

	// create index file if not exists
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		if _, err := os.Create(indexFile); err != nil {
			return fmt.Errorf("failed to create index file: %w", err)
		}
	}

	// create status file
	writer, err := newWriter(statusFile)
	if err != nil {
		return fmt.Errorf("failed to create status file: %w", err)
	}

	s.writer = writer

	return nil
}

// Write writes the current status to the active writer.
func (s *JSONDB) Write(status *model.Status) error {
	if err := s.writer.write(status); err != nil {
		return fmt.Errorf("failed to write status: %w", err)
	}
	return nil
}

// Close finalizes the current writer and compacts the status file.
func (s *JSONDB) Close() error {
	s.mu.Lock()

	if s.writer == nil {
		s.mu.Unlock()
		return nil
	}

	defer func() {
		// invalidate cache
		s.cache.Invalidate(s.writer.statusFile)

		// close the file
		if s.writer == nil {
			return
		}
		if err := s.writer.close(); err != nil {
			s.logger.Errorf("failed to close file %s: %v", s.writer.statusFile, err)
		}
		s.writer = nil
		s.mu.Unlock()
	}()

	// compact the file
	if err := s.Compact(s.writer.statusFile); err != nil {
		s.logger.Errorf("failed to compact file %s: %v", s.writer.statusFile, err)
	}

	return nil
}

// ReadStatusRecent retrieves the n most recent status files for a given DAG.
func (s *JSONDB) ReadStatusRecent(dagID string, n int) []*model.StatusFile {
	// Read the latest n status files for the given DAG.
	indexDir := craftIndexDataDir(s.baseDir, dagID)

	// If the index directory does not exist, return nil.
	if _, err := os.Stat(indexDir); os.IsNotExist(err) {
		return nil
	}

	// Search the index directory for the latest n status files.
	files, err := listFilesSorted(indexDir, true)
	if err != nil {
		s.logger.Errorf("failed to list files in %s: %v", indexDir, err)
		return nil
	}
	files = files[:min(n, len(files))]

	// Load the status of the latest n status files.
	var ret []*model.StatusFile
	for _, indexFile := range files {
		// Convert the index file to the status file.
		statusFilePattern, err := indexFileToStatusFilePattern(s.baseDir, indexFile)
		if err != nil {
			s.logger.Errorf("failed to convert index file to status file: %v", err)
			continue
		}

		// get the latest status file
		statusFiles, err := filepath.Glob(statusFilePattern)
		if err != nil {
			s.logger.Errorf("failed to list files in %s: %v", statusFilePattern, err)
			continue
		}
		statusFiles = getLatestFiles(statusFiles, 1)
		if len(statusFiles) == 0 {
			s.logger.Errorf("no status files found for %s", indexFile)
			continue
		}
		statusFile := statusFiles[0]

		// Load the latest status file
		status, err := s.cache.LoadLatest(statusFile, func() (*model.Status, error) {
			return ParseStatusFile(statusFile)
		})
		if err != nil {
			s.logger.Errorf("failed to parse file %s: %v", indexFile, err)
			continue
		}

		ret = append(ret, &model.StatusFile{
			File:   statusFile,
			Status: status,
		})
	}

	return ret
}

// listRecentFiles lists the most recent n status files in reverse chronological order.
func (s *JSONDB) listRecentFiles(path string, n int) ([]string, error) {
	var allFiles []string

	// Walk through the years in reverse order
	years, err := listDirsSorted(path, true)
	if err != nil {
		return nil, fmt.Errorf("error listing years: %w", err)
	}

	for _, year := range years {
		yearPath := filepath.Join(path, year)

		// Walk through the months in reverse order
		months, err := listDirsSorted(yearPath, true)
		if err != nil {
			return nil, fmt.Errorf("error listing months in %s: %w", year, err)
		}

		for _, month := range months {
			monthPath := filepath.Join(yearPath, month)

			// Walk through the days in reverse order
			days, err := listDirsSorted(monthPath, true)
			if err != nil {
				return nil, fmt.Errorf("error listing days in %s/%s: %w", year, month, err)
			}

			for _, day := range days {
				dayPath := filepath.Join(monthPath, day)

				// List files in the day directory
				files, err := listFilesSorted(dayPath, true)
				if err != nil {
					return nil, fmt.Errorf("error listing files in %s/%s/%s: %w", year, month, day, err)
				}

				allFiles = append(allFiles, files...)

				// If we have enough files, return them
				if len(allFiles) >= n {
					return allFiles[:n], nil
				}
			}
		}
	}

	// If we don't have enough files, return all we found
	if len(allFiles) > n {
		return allFiles[:n], nil
	}
	return allFiles, nil
}

// listDirsSorted lists directories in the given path, optionally in reverse order.
func listDirsSorted(path string, reverse bool) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}

	if reverse {
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	} else {
		sort.Strings(dirs)
	}

	return dirs, nil
}

// listFilesSorted lists files in the given path, optionally in reverse order.
func listFilesSorted(path string, reverse bool) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), extDat) {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}

	if reverse {
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
	} else {
		sort.Strings(files)
	}

	return files, nil
}

// ReadStatusToday retrieves the latest status file for today for a given DAG.
func (s *JSONDB) ReadStatusToday(dagID string) (*model.Status, error) {
	file, err := s.latestToday(dagID, time.Now(), s.latestStatusToday)
	if err != nil {
		return nil, err
	}

	return s.cache.LoadLatest(file, func() (*model.Status, error) {
		return ParseStatusFile(file)
	})
}

// FindByRequestID finds a status file by its request ID.
func (s *JSONDB) FindByRequestID(dagID string, reqID string) (*model.StatusFile, error) {
	if reqID == "" {
		return nil, fmt.Errorf("%w: requestID is empty", persistence.ErrRequestIDNotFound)
	}
	indexDir := craftIndexDataDir(s.baseDir, dagID)
	safeReqID := util.TruncString(reqID, requestIDLenSafe)
	pattern := filepath.Join(indexDir, "*"+safeReqID+"*.dat")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	// get the latest status file
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for _, f := range matches {
		pattern, err := indexFileToStatusFilePattern(s.baseDir, f)
		if err != nil {
			s.logger.Warn("failed to convert index file to status file: %s", err)
			continue
		}

		// get the latest status file
		statusFiles, err := filepath.Glob(pattern)
		if err != nil {
			s.logger.Warn("failed to list files in %s: %s", pattern, err)
			continue
		}

		for _, statusFile := range statusFiles {
			status, err := ParseStatusFile(statusFile)
			if err != nil {
				s.logger.Warn("parsing failed %s : %s", statusFile, err)
				continue
			}
			if status != nil && status.RequestID == reqID {
				return &model.StatusFile{File: statusFile, Status: status}, nil
			}
		}
	}

	return nil, fmt.Errorf("%w: %s", persistence.ErrRequestIDNotFound, reqID)
}

// RemoveAll removes all status files for a given DAG.
func (s *JSONDB) RemoveAll(dagID string) error {
	return s.RemoveOld(dagID, 0)
}

// RemoveOld removes status files older than the specified retention period.
func (s *JSONDB) RemoveOld(dagID string, retentionDays int) error {
	indexDir := craftIndexDataDir(s.baseDir, dagID)
	if retentionDays < 0 {
		return nil
	}
	pattern := filepath.Join(indexDir, "*.dat")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	expiredTime := time.Now().AddDate(0, 0, -retentionDays)
	var lastErr error
	for _, m := range matches {
		statusFilePattern, err := indexFileToStatusFilePattern(s.baseDir, m)
		if err != nil {
			s.logger.Warn("failed to convert index file to status file: %s", err)
			continue
		}
		statusFiles, err := filepath.Glob(statusFilePattern)
		if err != nil {
			s.logger.Warn("failed to list files in %s: %s", statusFilePattern, err)
			continue
		}
		latestStatusFiles := getLatestFiles(statusFiles, 1)
		if len(latestStatusFiles) == 0 {
			s.logger.Warn("no status files found for %s", m)
			continue
		}
		info, err := os.Stat(latestStatusFiles[0])
		if err != nil {
			s.logger.Warn("failed to get file info %s: %s", latestStatusFiles[0], err)
			continue
		}
		if info.ModTime().After(expiredTime) {
			// skip if the file is not expired
			continue
		}
		// Remove the status file and the index file
		if err := os.Remove(m); err != nil {
			s.logger.Warn("failed to remove %s: %s", m, err)
			lastErr = err
		}
		for _, f := range statusFiles {
			if err := os.Remove(f); err != nil {
				s.logger.Warn("failed to remove %s: %s", f, err)
				lastErr = err
			}
		}
	}
	return lastErr
}

// Compact compresses the status file by keeping only the latest status.
func (s *JSONDB) Compact(statusFile string) error {
	status, err := ParseStatusFile(statusFile)
	if err == io.EOF {
		// no data to compact
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to parse file %s: %w", statusFile, err)
	}

	compactedFile, err := craftCompactedFileName(statusFile)
	if err != nil {
		return err
	}

	w := &writer{statusFile: compactedFile}
	if err := w.open(); err != nil {
		return err
	}
	defer w.close()

	if err := w.write(status); err != nil {
		// rollback
		if removeErr := os.Remove(compactedFile); removeErr != nil {
			log.Printf("failed to remove %s : %s", compactedFile, removeErr)
		}
		return err
	}

	return os.Remove(statusFile)
}

// Rename changes the ID of a DAG, effectively renaming its associated files.
func (s *JSONDB) Rename(oldID, newID string) error {
	if oldID == newID {
		return nil
	}

	oldIndexDir := craftIndexDataDir(s.baseDir, oldID)
	newIndexDir := craftIndexDataDir(s.baseDir, newID)

	if !pathExists(oldIndexDir) {
		return nil
	}

	// Check the new directory does not exist.
	// If it does, return an error.
	if pathExists(newIndexDir) {
		return fmt.Errorf("%w: %s", persistence.ErrConflict, newID)
	}

	// Rename the index directory.
	if err := os.Rename(oldIndexDir, newIndexDir); err != nil {
		return fmt.Errorf("failed to rename index directory: %w", err)
	}

	return nil
}

// latestToday finds the latest status file for today or the most recent day.
func (s *JSONDB) latestToday(dagID string, day time.Time, latestStatusToday bool) (string, error) {
	indexDir := craftIndexDataDir(s.baseDir, dagID)

	// Search the today's status directory for the latest status file.
	var pattern string
	if latestStatusToday {
		pattern = filepath.Join(indexDir, normalizedID(dagID)+"*"+day.Format(dateFormat)+"*.dat")
	} else {
		pattern = filepath.Join(indexDir, normalizedID(dagID)+"*.dat")
	}

	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", persistence.ErrNoStatusDataToday
	}

	latestFiles := getLatestFiles(matches, 1)
	if len(latestFiles) == 0 {
		return "", persistence.ErrNoStatusData
	}
	return s.indexFileToStatusFile(latestFiles[0])
}

// indexFileToStatusFile converts an index file path to its corresponding status file path.
func (s *JSONDB) indexFileToStatusFile(indexFile string) (string, error) {
	pattern, err := indexFileToStatusFilePattern(s.baseDir, indexFile)
	if err != nil {
		return "", err
	}
	files, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	files = getLatestFiles(files, 1)
	if len(files) == 0 {
		return "", fmt.Errorf("no status files found for %s", indexFile)
	}
	return files[0], nil
}

// ReadStatusForDate retrieves all status files for a given DAG on a specific date.
func (s *JSONDB) ReadStatusForDate(dagID string, date time.Time) ([]*model.StatusFile, error) {
	indexDir := craftIndexDataDir(s.baseDir, dagID)
	dateStr := date.Format(dateFormat)
	pattern := filepath.Join(indexDir, normalizedID(dagID)+"*"+dateStr+"*.dat")

	indexFiles, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list index files: %w", err)
	}

	var statusFiles []*model.StatusFile
	for _, indexFile := range indexFiles {
		statusFilePattern, err := indexFileToStatusFilePattern(s.baseDir, indexFile)
		if err != nil {
			s.logger.Errorf("failed to convert index file to status file pattern: %v", err)
			continue
		}

		matches, err := filepath.Glob(statusFilePattern)
		if err != nil {
			s.logger.Errorf("failed to list status files: %v", err)
			continue
		}

		for _, statusFile := range matches {
			status, err := s.cache.LoadLatest(statusFile, func() (*model.Status, error) {
				return ParseStatusFile(statusFile)
			})
			if err != nil {
				s.logger.Errorf("failed to parse file %s: %v", statusFile, err)
				continue
			}

			statusFiles = append(statusFiles, &model.StatusFile{
				File:   statusFile,
				Status: status,
			})
		}
	}

	// Sort status files by timestamp in descending order
	sort.Slice(statusFiles, func(i, j int) bool {
		return strings.Compare(statusFiles[i].Status.StartedAt, statusFiles[j].Status.StartedAt) > 0
	})

	return statusFiles, nil
}

// pathExists checks if a given path exists.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// ParseStatusFile reads and parses a status file, returning the latest status.
func ParseStatusFile(file string) (*model.Status, error) {
	f, err := os.Open(file)
	if err != nil {
		log.Printf("failed to open file. err: %v", err)
		return nil, err
	}
	defer f.Close()

	var (
		offset int64
		ret    *model.Status
	)
	for {
		line, err := readLineFrom(f, offset)
		if err == io.EOF {
			if ret == nil {
				return nil, err
			}
			return ret, nil
		} else if err != nil {
			return nil, err
		}
		offset += int64(len(line)) + 1 // +1 for newline
		if len(line) > 0 {
			m, err := model.StatusFromJSON(string(line))
			if err == nil {
				ret = m
			}
		}
	}
}

// getLatestFiles returns the n most recent files from a given list.
func getLatestFiles(files []string, n int) []string {
	if len(files) == 0 {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files[:min(n, len(files))]
}

// readLineFrom reads a line from a file starting at a specific offset.
func readLineFrom(f *os.File, offset int64) ([]byte, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	var ret []byte
	for {
		b, isPrefix, err := r.ReadLine()
		if err != nil {
			return ret, err
		}
		ret = append(ret, b...)
		if !isPrefix {
			break
		}
	}
	return ret, nil
}

const (
	compactedFileSuffix = "_c.dat"
)

// craftCompactedFileName creates a filename for a compacted status file.
func craftCompactedFileName(file string) (string, error) {
	if strings.HasSuffix(file, compactedFileSuffix) {
		return "", persistence.ErrFileIsCompacted
	}
	return filepath.Join(
		filepath.Dir(file),
		strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))+
			compactedFileSuffix,
	), nil
}

// craftIndexDataDir constructs the path to the index directory for a DAG.
func craftIndexDataDir(baseDir string, dagID string) string {
	return filepath.Join(baseDir, "index", normalizedID(dagID))
}

// craftStatusDataDir constructs the path to the status directory for a specific date.
func craftStatusDataDir(baseDir string, t time.Time) string {
	year := t.Format("2006")
	month := t.Format("01")
	date := t.Format("02")
	return filepath.Join(baseDir, "status", year, month, date)
}

// craftStatusFile generates a filename for a status file.
func craftStatusFile(dagID, requestID string, t time.Time) string {
	// status file name format: <dagID>.<timestamp>.<requestID>.dat
	return fmt.Sprintf("%s.%s.%s.dat",
		normalizedID(dagID),
		t.Format(dateTimeFormat),
		util.TruncString(requestID, requestIDLenSafe),
	)
}

// indexFileToStatusFilePattern converts an index file path to a pattern for finding corresponding status files.
func indexFileToStatusFilePattern(baseDir, indexFile string) (string, error) {
	indexFileInfo, err := parseIndexFile(indexFile)
	if err != nil {
		return "", err
	}
	baseName := strings.TrimSuffix(filepath.Base(indexFile), filepath.Ext(indexFile))
	return filepath.Join(
		baseDir,
		"status",
		indexFileInfo.year, indexFileInfo.month, indexFileInfo.date,
		baseName+"*.dat",
	), nil
}

var (
	indexFileRegExp = regexp.MustCompile(`(\d{4})(\d{2})(\d{2})\.\d{2}:\d{2}:\d{2}.\d{3}\.([^.]+)\.dat`)
)

// indexFileInfo holds information parsed from an index file name.
type indexFileInfo struct {
	filePath string
	year     string
	month    string
	date     string
	reqID    string
}

// parseIndexFile extracts information from an index file name.
func parseIndexFile(indexFile string) (indexFileInfo, error) {
	m := indexFileRegExp.FindStringSubmatch(indexFile)
	if len(m) != 5 {
		return indexFileInfo{}, fmt.Errorf("invalid index file: %s", indexFile)
	}
	return indexFileInfo{
		filePath: indexFile,
		year:     m[1],
		month:    m[2],
		date:     m[3],
		reqID:    m[4],
	}, nil
}

// normalizedID creates a valid filename from a DAG ID.
func normalizedID(dagID string) string {
	return util.ValidFilename(
		strings.TrimSuffix(filepath.Base(dagID), filepath.Ext(dagID)),
	)
}
