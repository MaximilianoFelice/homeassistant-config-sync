// Package scan walks the live disk and builds the set of managed files with
// their git blobs (design.md §5, scan safety guards). It writes nothing:
// neither to disk nor to git.
package scan

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // it is git's object format, not a cryptographic use
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
)

// Git modes for regular files.
const (
	ModeFile = "100644"
	ModeExec = "100755"
)

// binarySniff is how many bytes are inspected to detect a binary.
const binarySniff = 8 << 10

// readAttempts is how many times reading a file that changes while being
// read is attempted.
const readAttempts = 3

// racyWindow: a file whose mtime falls within this window of the scan start
// is not stored in the cache, because an edit within the same clock tick
// might not change its stat.
const racyWindow = 2 * time.Second

// SkipReason explains why a managed file did not make it into the snapshot.
type SkipReason string

const (
	SkipTooLarge   SkipReason = "too_large"
	SkipBinary     SkipReason = "binary"
	SkipSymlink    SkipReason = "symlink"
	SkipNotRegular SkipReason = "not_regular"
	SkipUnstable   SkipReason = "unstable"
	SkipUnreadable SkipReason = "unreadable"
)

// Skipped is a managed path left out of this scan. The snapshot keeps the
// previous version: skipping never amounts to deleting.
type Skipped struct {
	RepoPath string     `json:"repo_path"`
	BoxPath  string     `json:"box_path"`
	Reason   SkipReason `json:"reason"`
	// Dir means an entire (unreadable) directory was skipped: everything
	// under RepoPath keeps its previous version.
	Dir bool `json:"dir,omitempty"`
}

// Covers says whether the skip applies to a repo path.
func (s Skipped) Covers(repoPath string) bool {
	if s.Dir {
		return len(repoPath) > len(s.RepoPath) && repoPath[:len(s.RepoPath)+1] == s.RepoPath+"/"
	}
	return repoPath == s.RepoPath
}

// File is a managed file present on the live disk.
type File struct {
	RepoPath  string
	BoxPath   string
	Mode      string
	OID       string
	Size      int64
	Direction paths.Direction
	// Content is the content read; it is nil if the OID came from the cache.
	Content []byte
}

// Root is the state of a mapping root.
type Root struct {
	Mapping int
	Box     string
	// Active says whether the mapping manages anything (has include or push_only).
	Active bool
	// Exists says whether the root exists and is a directory.
	Exists bool
	// Managed is how many managed files were found.
	Managed int
}

// CacheEntry stores a file's stat and its blob, so what did not change
// between cycles is not re-hashed.
type CacheEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
	Ino     uint64 `json:"ino"`
	Mode    string `json:"mode"`
	OID     string `json:"oid"`
}

// Cache indexes CacheEntry by repo path.
type Cache map[string]CacheEntry

// Result is the result of a scan.
type Result struct {
	Files   map[string]File
	Skipped []Skipped
	Roots   []Root
	// Cache is the new cache, to pass to the next scan.
	Cache Cache
}

// Options configures a scan.
type Options struct {
	MaxFileSize int64
	Cache       Cache
	Now         func() time.Time

	// afterRead is a test hook that runs between reading a file and the
	// control stat.
	afterRead func(boxPath string, attempt int)
}

// Scan walks the roots of the active mappings.
func Scan(m *paths.Matcher, opts Options) (*Result, error) {
	if opts.MaxFileSize <= 0 {
		return nil, errors.New("MaxFileSize must be greater than 0")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	s := &scanner{
		m:     m,
		opts:  opts,
		start: now(),
		res:   &Result{Files: map[string]File{}, Cache: Cache{}},
	}
	for i, mp := range m.Mappings() {
		root := Root{Mapping: i, Box: mp.Box, Active: len(mp.Include)+len(mp.PushOnly) > 0}
		if root.Active {
			if fi, err := os.Stat(mp.Box); err == nil && fi.IsDir() {
				root.Exists = true
				if err := s.walk(i, mp.Box, &root); err != nil {
					return nil, err
				}
			}
		}
		s.res.Roots = append(s.res.Roots, root)
	}
	sort.Slice(s.res.Skipped, func(a, b int) bool { return s.res.Skipped[a].RepoPath < s.res.Skipped[b].RepoPath })
	return s.res, nil
}

type scanner struct {
	m     *paths.Matcher
	opts  Options
	start time.Time
	res   *Result
}

func (s *scanner) walk(i int, rootDir string, root *Root) error {
	return filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == rootDir {
				return err
			}
			// An unreadable subdirectory does not stop the scan; whatever was
			// inside keeps its previous version (it is not deleted).
			r := s.m.ClassifyBoxPath(p)
			isDir := d != nil && d.IsDir()
			if r.RepoPath != "" && (isDir || r.Managed) {
				s.res.Skipped = append(s.res.Skipped, Skipped{RepoPath: r.RepoPath, BoxPath: r.BoxPath, Reason: SkipUnreadable, Dir: isDir})
			}
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == rootDir {
				return nil
			}
			rel, rerr := filepath.Rel(rootDir, p)
			if rerr == nil && s.m.PruneDir(i, filepath.ToSlash(rel)) {
				return fs.SkipDir
			}
			return nil
		}

		r := s.m.ClassifyBoxPath(p)
		if !r.Managed {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			s.skip(r, SkipSymlink)
			return nil
		case !d.Type().IsRegular():
			s.skip(r, SkipNotRegular)
			return nil
		}
		root.Managed++
		s.file(r)
		return nil
	})
}

func (s *scanner) skip(r paths.Result, reason SkipReason) {
	s.res.Skipped = append(s.res.Skipped, Skipped{RepoPath: r.RepoPath, BoxPath: r.BoxPath, Reason: reason})
}

func (s *scanner) file(r paths.Result) {
	fi, err := os.Lstat(r.BoxPath)
	if err != nil {
		s.skip(r, SkipUnreadable)
		return
	}
	if fi.Size() > s.opts.MaxFileSize {
		s.skip(r, SkipTooLarge)
		return
	}
	mode := ModeFile
	if fi.Mode().Perm()&0o111 != 0 {
		mode = ModeExec
	}
	st := statOf(fi)

	if c, ok := s.opts.Cache[r.RepoPath]; ok && c == (CacheEntry{Size: st.size, MtimeNs: st.mtimeNs, CtimeNs: st.ctimeNs, Ino: st.ino, Mode: mode, OID: c.OID}) {
		s.res.Files[r.RepoPath] = File{RepoPath: r.RepoPath, BoxPath: r.BoxPath, Mode: mode, OID: c.OID, Size: st.size, Direction: r.Direction}
		s.res.Cache[r.RepoPath] = c
		return
	}

	content, st, err := s.readStable(r.BoxPath)
	switch {
	case errors.Is(err, errUnstable):
		s.skip(r, SkipUnstable)
		return
	case err != nil:
		s.skip(r, SkipUnreadable)
		return
	case int64(len(content)) > s.opts.MaxFileSize:
		s.skip(r, SkipTooLarge)
		return
	case bytes.IndexByte(content[:min(len(content), binarySniff)], 0) >= 0:
		s.skip(r, SkipBinary)
		return
	}

	oid := BlobOID(content)
	s.res.Files[r.RepoPath] = File{RepoPath: r.RepoPath, BoxPath: r.BoxPath, Mode: mode, OID: oid, Size: st.size, Direction: r.Direction, Content: content}
	if s.start.Sub(time.Unix(0, st.mtimeNs)) > racyWindow {
		s.res.Cache[r.RepoPath] = CacheEntry{Size: st.size, MtimeNs: st.mtimeNs, CtimeNs: st.ctimeNs, Ino: st.ino, Mode: mode, OID: oid}
	}
}

var errUnstable = errors.New("the file changed while it was being read")

// readStable reads a file and confirms with a later stat that it did not
// change during the read.
func (s *scanner) readStable(p string) ([]byte, stat, error) {
	for attempt := range readAttempts {
		fi1, err := os.Lstat(p)
		if err != nil {
			return nil, stat{}, err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, stat{}, err
		}
		if s.opts.afterRead != nil {
			s.opts.afterRead(p, attempt)
		}
		fi2, err := os.Lstat(p)
		if err != nil {
			return nil, stat{}, err
		}
		st1, st2 := statOf(fi1), statOf(fi2)
		if st1 == st2 && st2.size == int64(len(content)) {
			return content, st2, nil
		}
	}
	return nil, stat{}, errUnstable
}

type stat struct {
	size    int64
	mtimeNs int64
	ctimeNs int64
	ino     uint64
}

func statOf(fi fs.FileInfo) stat {
	ctime, ino := sysStat(fi)
	return stat{size: fi.Size(), mtimeNs: fi.ModTime().UnixNano(), ctimeNs: ctime, ino: ino}
}

// BlobOID computes the SHA-1 OID git would give a blob with that content.
func BlobOID(content []byte) string {
	h := sha1.New() //nolint:gosec // git's object format
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// ReadVerified re-reads a file and confirms its blob is still oid. It is used
// when the scan took the OID from the cache and the content is needed.
func ReadVerified(boxPath, oid string, maxSize int64) ([]byte, error) {
	fi, err := os.Lstat(boxPath)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxSize {
		return nil, fmt.Errorf("%s is no longer a regular file of valid size", boxPath)
	}
	content, err := os.ReadFile(boxPath)
	if err != nil {
		return nil, err
	}
	if got := BlobOID(content); got != oid {
		return nil, fmt.Errorf("%s: %w", boxPath, errUnstable)
	}
	return content, nil
}

// IsUnstable says whether an error came from a file that changed while it
// was being read.
func IsUnstable(err error) bool { return errors.Is(err, errUnstable) }
