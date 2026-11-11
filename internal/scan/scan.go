// Package scan recorre el disco vivo y arma el conjunto de archivos
// gestionados con su blob de git (design.md §5, "Guards de seguridad del
// escaneo"). No escribe nada: ni en disco ni en git.
package scan

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // es el formato de objetos de git, no un uso criptográfico
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

// Modos de git para archivos regulares.
const (
	ModeFile = "100644"
	ModeExec = "100755"
)

// binarySniff es cuántos bytes se miran para detectar un binario.
const binarySniff = 8 << 10

// readAttempts es cuántas veces se intenta leer un archivo que cambia
// mientras se lee.
const readAttempts = 3

// racyWindow: un archivo cuyo mtime cae dentro de esta ventana respecto del
// inicio del escaneo no se guarda en la cache, porque una edición en el mismo
// tick de reloj podría no cambiar su stat.
const racyWindow = 2 * time.Second

// SkipReason explica por qué un archivo gestionado no entró al snapshot.
type SkipReason string

const (
	SkipTooLarge   SkipReason = "too_large"
	SkipBinary     SkipReason = "binary"
	SkipSymlink    SkipReason = "symlink"
	SkipNotRegular SkipReason = "not_regular"
	SkipUnstable   SkipReason = "unstable"
	SkipUnreadable SkipReason = "unreadable"
)

// Skipped es un path gestionado que se dejó afuera de este escaneo. El
// snapshot conserva la versión anterior: saltear nunca equivale a borrar.
type Skipped struct {
	RepoPath string
	BoxPath  string
	Reason   SkipReason
	// Dir indica que se salteó un directorio entero (ilegible): todo lo que
	// esté debajo de RepoPath conserva su versión anterior.
	Dir bool
}

// Covers dice si el salteo alcanza a un path del repo.
func (s Skipped) Covers(repoPath string) bool {
	if s.Dir {
		return len(repoPath) > len(s.RepoPath) && repoPath[:len(s.RepoPath)+1] == s.RepoPath+"/"
	}
	return repoPath == s.RepoPath
}

// File es un archivo gestionado presente en el disco vivo.
type File struct {
	RepoPath  string
	BoxPath   string
	Mode      string
	OID       string
	Size      int64
	Direction paths.Direction
	// Content es el contenido leído; es nil si el OID salió de la cache.
	Content []byte
}

// Root es el estado de la raíz de un mapeo.
type Root struct {
	Mapping int
	Box     string
	// Active indica si el mapeo gestiona algo (tiene include o push_only).
	Active bool
	// Exists indica si la raíz existe y es un directorio.
	Exists bool
	// Managed es cuántos archivos gestionados se encontraron.
	Managed int
}

// CacheEntry guarda el stat de un archivo y su blob, para no re-hashear lo
// que no cambió entre ciclos.
type CacheEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
	Ino     uint64 `json:"ino"`
	Mode    string `json:"mode"`
	OID     string `json:"oid"`
}

// Cache indexa CacheEntry por path del repo.
type Cache map[string]CacheEntry

// Result es el resultado de un escaneo.
type Result struct {
	Files   map[string]File
	Skipped []Skipped
	Roots   []Root
	// Cache es la cache nueva, para pasarla al próximo escaneo.
	Cache Cache
}

// Options configura un escaneo.
type Options struct {
	MaxFileSize int64
	Cache       Cache
	Now         func() time.Time

	// afterRead es un hook de tests que corre entre la lectura de un archivo
	// y el stat de control.
	afterRead func(boxPath string, attempt int)
}

// Scan recorre las raíces de los mapeos activos.
func Scan(m *paths.Matcher, opts Options) (*Result, error) {
	if opts.MaxFileSize <= 0 {
		return nil, errors.New("MaxFileSize tiene que ser mayor que 0")
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
			// Un subdirectorio ilegible no frena el escaneo; lo que hubiera
			// adentro conserva su versión anterior (no se borra).
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

var errUnstable = errors.New("el archivo cambió mientras se leía")

// readStable lee un archivo y confirma con un stat posterior que no cambió
// durante la lectura.
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

// BlobOID calcula el OID SHA-1 que git le daría a un blob con ese contenido.
func BlobOID(content []byte) string {
	h := sha1.New() //nolint:gosec // formato de objetos de git
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// ReadVerified vuelve a leer un archivo y confirma que su blob sigue siendo
// oid. Se usa cuando el escaneo sacó el OID de la cache y hace falta el
// contenido.
func ReadVerified(boxPath, oid string, maxSize int64) ([]byte, error) {
	fi, err := os.Lstat(boxPath)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxSize {
		return nil, fmt.Errorf("%s ya no es un archivo regular de tamaño válido", boxPath)
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

// IsUnstable dice si un error vino de un archivo que cambió durante la lectura.
func IsUnstable(err error) bool { return errors.Is(err, errUnstable) }
