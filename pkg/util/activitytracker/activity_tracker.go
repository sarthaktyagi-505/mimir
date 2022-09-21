// SPDX-License-Identifier: AGPL-3.0-only

package activitytracker

import (
	"encoding/binary"
	"flag"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edsrzf/mmap-go"
	"github.com/grafana/dskit/multierror"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ActivityTracker remembers active tasks in the file. If file already exists, it is recreated from scratch.
// Activity tracker uses mmap to write to the file, which allows fast writes because it's only using memory access,
// with no system calls.
// Nil activity tracker ignores all calls to its public API.
type ActivityTracker struct {
	file           *os.File
	fileBytes      mmap.MMap
	freeIndexQueue chan int // Used as a queue for indexes of free entries.
	maxEntries     int

	failedInserts       *prometheus.CounterVec
	freeActivityEntries prometheus.GaugeFunc
}

const (
	entrySize int = 1024

	reasonFull          = "tracker_full"
	reasonEmptyActivity = "empty_activity"
)

var emptyEntry = make([]byte, entrySize)

type Config struct {
	Filepath   string `yaml:"filepath"`
	MaxEntries int    `yaml:"max_entries" category:"advanced"`
}

func (c *Config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&c.Filepath, "activity-tracker.filepath", "./metrics-activity.log", "File where ongoing activities are stored. If empty, activity tracking is disabled.")
	f.IntVar(&c.MaxEntries, "activity-tracker.max-entries", 1024, "Max number of concurrent activities that can be tracked. Used to size the file in advance. Additional activities are ignored.")
}

func NewActivityTracker(cfg Config, reg prometheus.Registerer) (*ActivityTracker, error) {
	if cfg.Filepath == "" {
		return nil, nil
	}

	filesize := cfg.MaxEntries * entrySize
	file, fileAsBytes, err := getMappedFile(cfg.Filepath, filesize)
	if err != nil {
		return nil, err
	}

	tracker := &ActivityTracker{
		file:           file,
		fileBytes:      fileAsBytes,
		freeIndexQueue: make(chan int, cfg.MaxEntries),
		maxEntries:     cfg.MaxEntries,

		failedInserts: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "activity_tracker_failed_total",
			Help: "How many times has activity tracker failed to insert new activity.",
		}, []string{"reason"}),
	}

	tracker.failedInserts.WithLabelValues(reasonFull)
	tracker.failedInserts.WithLabelValues(reasonEmptyActivity)

	tracker.freeActivityEntries = promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "activity_tracker_free_slots",
		Help: "Number of free slots in activity file.",
	}, func() float64 {
		return float64(len(tracker.freeIndexQueue))
	})

	for i := 0; i < cfg.MaxEntries; i++ {
		tracker.freeIndexQueue <- i
	}

	return tracker, nil
}

// Timestamp is encoded as uint64 value returned by time.UnixNano.
const timestampLength = 8

// Insert inserts entry (generated by activityGenerator) into the activity tracker. If tracker
// is full, activityGenerator is not called. Value returned by Insert is to be used with Delete method
// after activity has finished.
//
// String returned by activityGenerator should be human-readable description of activity.
// If it is bigger than max entry size, it will be trimmed on latest utf-8 rune start before the limit.
//
// Note that timestamp of Insert call is stored automatically with the tracked activity.
func (t *ActivityTracker) Insert(activityGenerator func() string) (activityIndex int) {
	if t == nil {
		return -1
	}

	select {
	case i := <-t.freeIndexQueue:
		activity := activityGenerator()
		if activity == "" {
			t.freeIndexQueue <- i
			t.failedInserts.WithLabelValues(reasonEmptyActivity).Inc()
			return -1
		}

		ix := i * entrySize
		binary.BigEndian.PutUint64(t.fileBytes[ix:], uint64(time.Now().UnixNano()))

		activity = trimEntryToSize(activity, entrySize-timestampLength)

		copy(t.fileBytes[ix+timestampLength:], activity)
		return i
	default:
		t.failedInserts.WithLabelValues(reasonFull).Inc()
		return -1
	}
}

func (t *ActivityTracker) InsertStatic(activity string) (activityIndex int) {
	return t.Insert(func() string { return activity })
}

// Delete removes activity with given index (returned previously by Insert) from the tracker.
// Should only be called once for each activity, as indexes are reused.
// It is OK to call Delete with negative index, which is returned by Insert when activity couldn't be inserted.
func (t *ActivityTracker) Delete(activityIndex int) {
	if activityIndex < 0 || activityIndex >= t.maxEntries {
		return
	}

	copy(t.fileBytes[activityIndex*entrySize:], emptyEntry)
	t.freeIndexQueue <- activityIndex
}

// Close closes activity tracker. Calling other methods after Close() will likely panic. Don't do that.
func (t *ActivityTracker) Close() error {
	if t == nil {
		return nil
	}

	err1 := t.fileBytes.Unmap()
	err2 := t.file.Close()

	return multierror.New(err1, err2).Err()
}

// Trim entry to given size limit, respecting UTF-8 rune boundaries.
func trimEntryToSize(entry string, size int) string {
	if len(entry) <= size {
		return entry
	}

	l := size
	for l > 0 && !utf8.RuneStart(entry[l]) {
		l--
	}

	return entry[:l]
}

func getMappedFile(filename string, filesize int) (*os.File, mmap.MMap, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create activity file")
	}

	closeOnReturn := true
	defer func() {
		if closeOnReturn {
			_ = file.Close()
		}
	}()

	err = file.Truncate(int64(filesize))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to truncate activity file")
	}

	fileAsBytes, err := mmap.Map(file, mmap.RDWR, 0)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to mmap activity file")
	}

	closeOnReturn = false
	return file, fileAsBytes, err
}

// Entry describes activity in the tracker.
type Entry struct {
	Timestamp time.Time
	Activity  string
}

// LoadUnfinishedEntries loads and returns list of unfinished activities in the activity file.
func LoadUnfinishedEntries(file string) ([]Entry, error) {
	fd, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	defer func() { _ = fd.Close() }()

	var results []Entry

	buf := make([]byte, entrySize)
	var n int
	for n, err = io.ReadFull(fd, buf); err == nil; _, err = io.ReadFull(fd, buf) {
		s := string(buf[:n])
		if s == string(emptyEntry) {
			continue
		}

		var ts = time.Unix(0, int64(binary.BigEndian.Uint64(buf)))

		s = s[timestampLength:]
		s = strings.ReplaceAll(s, "\x00", "")

		results = append(results, Entry{
			Timestamp: ts,
			Activity:  s,
		})
	}

	// io.ReadFull returns io.EOF if it reads no more bytes. This is good.
	if errors.Is(err, io.EOF) {
		err = nil
	}

	return results, err
}
