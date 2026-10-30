package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/emersion/go-webdav/internal"
)

// LocalFileSystem implements FileSystem for a local directory. Every access
// goes through os.Root, so neither ".." nor a symlink can reach outside it.
type LocalFileSystem string

var _ FileSystem = LocalFileSystem("")

// localPath maps an absolute WebDAV path to a slash-separated name relative to
// the root.
func localPath(name string) (string, error) {
	if (filepath.Separator != '/' && strings.IndexRune(name, filepath.Separator) >= 0) || strings.Contains(name, "\x00") {
		return "", internal.HTTPErrorf(http.StatusBadRequest, "webdav: invalid character in path")
	}
	name = path.Clean(name)
	if !path.IsAbs(name) {
		return "", internal.HTTPErrorf(http.StatusBadRequest, "webdav: expected absolute path, got %q", name)
	}
	if name == "/" {
		return ".", nil
	}
	return name[1:], nil
}

func (local LocalFileSystem) openRoot() (*os.Root, error) {
	r, err := os.OpenRoot(string(local))
	return r, errFromOS(err)
}

func (local LocalFileSystem) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	p, err := localPath(name)
	if err != nil {
		return nil, err
	}
	r, err := local.openRoot()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(p)
	if err != nil {
		return nil, errFromOS(err)
	}
	return f, nil
}

func fileInfoFromOS(p string, fi os.FileInfo) *FileInfo {
	return &FileInfo{
		Path:    p,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
		IsDir:   fi.IsDir(),
		// TODO: fallback to http.DetectContentType?
		MIMEType: mime.TypeByExtension(path.Ext(p)),
		// RFC 2616 section 13.3.3 describes strong ETags. Ideally these would
		// be checksums or sequence numbers, however these are expensive to
		// compute. The modification time with nanosecond granularity is good
		// enough, as it's very unlikely for the same file to be modified twice
		// during a single nanosecond.
		ETag: fmt.Sprintf("%x%x", fi.ModTime().UnixNano(), fi.Size()),
	}
}

func errFromOS(err error) error {
	// Remove path from path errors so it's not returned to the user
	var perr *fs.PathError
	if errors.As(err, &perr) {
		err = fmt.Errorf("%s: %w", perr.Op, perr.Err)
	}
	var lerr *os.LinkError
	if errors.As(err, &lerr) {
		err = fmt.Errorf("%s: %w", lerr.Op, lerr.Err)
	}

	if errors.Is(err, fs.ErrNotExist) {
		return NewHTTPError(http.StatusNotFound, err)
	} else if errors.Is(err, fs.ErrPermission) {
		return NewHTTPError(http.StatusForbidden, err)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		return NewHTTPError(http.StatusServiceUnavailable, err)
	} else {
		return err
	}
}

func (local LocalFileSystem) Stat(ctx context.Context, name string) (*FileInfo, error) {
	p, err := localPath(name)
	if err != nil {
		return nil, err
	}
	r, err := local.openRoot()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	fi, err := r.Stat(p)
	if err != nil {
		return nil, errFromOS(err)
	}
	return fileInfoFromOS(name, fi), nil
}

func (local LocalFileSystem) ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error) {
	dir, err := localPath(name)
	if err != nil {
		return nil, err
	}
	r, err := local.openRoot()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var l []FileInfo
	err = fs.WalkDir(r.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}

		l = append(l, *fileInfoFromOS(path.Join("/", p), fi))

		if !recursive && fi.IsDir() && dir != p {
			return fs.SkipDir
		}
		return nil
	})
	return l, errFromOS(err)
}

func checkConditionalMatches(fi *FileInfo, ifMatch, ifNoneMatch ConditionalMatch) error {
	etag := ""
	if fi != nil {
		etag = fi.ETag
	}

	if ifMatch.IsSet() {
		if ok, err := ifMatch.MatchETag(etag); err != nil {
			return NewHTTPError(http.StatusBadRequest, err)
		} else if !ok {
			return NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-Match condition failed"))
		}
	}

	if ifNoneMatch.IsSet() {
		if ok, err := ifNoneMatch.MatchETag(etag); err != nil {
			return NewHTTPError(http.StatusBadRequest, err)
		} else if ok {
			return NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-None-Match condition failed"))
		}
	}

	return nil
}

func (local LocalFileSystem) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (fi *FileInfo, created bool, err error) {
	p, err := localPath(name)
	if err != nil {
		return nil, false, err
	}
	r, err := local.openRoot()
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	fi, _ = local.Stat(ctx, name)
	created = fi == nil

	if err := checkConditionalMatches(fi, opts.IfMatch, opts.IfNoneMatch); err != nil {
		return nil, false, err
	}

	wc, err := r.Create(p)
	if err != nil {
		return nil, false, errFromOS(err)
	}
	defer wc.Close()

	if _, err := io.Copy(wc, body); err != nil {
		r.Remove(p)
		return nil, false, err
	}
	if err := wc.Close(); err != nil {
		r.Remove(p)
		return nil, false, err
	}

	fi, err = local.Stat(ctx, name)
	if err != nil {
		return nil, false, err
	}

	return fi, created, err
}

func (local LocalFileSystem) RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error {
	p, err := localPath(name)
	if err != nil {
		return err
	}
	r, err := local.openRoot()
	if err != nil {
		return err
	}
	defer r.Close()

	// WebDAV semantics are that it should return a "404 Not Found" error in
	// case the resource doesn't exist. We need to Stat before RemoveAll.
	fi, err := local.Stat(ctx, name)
	if err != nil {
		return errFromOS(err)
	}

	if err := checkConditionalMatches(fi, opts.IfMatch, opts.IfNoneMatch); err != nil {
		return err
	}

	return errFromOS(r.RemoveAll(p))
}

func (local LocalFileSystem) Mkdir(ctx context.Context, name string) error {
	p, err := localPath(name)
	if err != nil {
		return err
	}
	r, err := local.openRoot()
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Mkdir(p, 0755); os.IsExist(err) {
		return NewHTTPError(http.StatusMethodNotAllowed, err)
	} else {
		return errFromOS(err)
	}
}

func copyRegularFile(r *os.Root, src, dst string, perm os.FileMode) error {
	srcFile, err := r.Open(src)
	if err != nil {
		return errFromOS(err)
	}
	defer srcFile.Close()

	dstFile, err := r.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if os.IsNotExist(err) {
		return NewHTTPError(http.StatusConflict, err)
	} else if err != nil {
		return errFromOS(err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Close()
}

// copyMovePaths resolves src and dst and refuses a destination that is the
// source or inside it: replacing it would delete the source first, and a
// recursive copy into itself never ends. Comparing file identity of dst and
// each ancestor catches aliases through symlinks, which path strings miss.
func copyMovePaths(r *os.Root, src, dst string) (string, string, error) {
	srcPath, err := localPath(src)
	if err != nil {
		return "", "", err
	}
	dstPath, err := localPath(dst)
	if err != nil {
		return "", "", err
	}
	srcInfo, err := r.Stat(srcPath)
	if err != nil {
		return "", "", errFromOS(err)
	}
	for p := dstPath; ; p = path.Dir(p) {
		if fi, err := r.Stat(p); err == nil && os.SameFile(fi, srcInfo) {
			return "", "", internal.HTTPErrorf(http.StatusForbidden, "webdav: destination is the source or inside it")
		}
		if p == "." {
			return srcPath, dstPath, nil
		}
	}
}

// prepareDestination reports whether dst is new, removing it if it exists and
// may be overwritten.
func prepareDestination(r *os.Root, dst string, noOverwrite bool) (created bool, err error) {
	if _, err := r.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			return false, errFromOS(err)
		}
		return true, nil
	}
	if noOverwrite {
		return false, NewHTTPError(http.StatusPreconditionFailed, os.ErrExist)
	}
	return false, errFromOS(r.RemoveAll(dst))
}

func (local LocalFileSystem) Copy(ctx context.Context, src, dst string, options *CopyOptions) (created bool, err error) {
	r, err := local.openRoot()
	if err != nil {
		return false, err
	}
	defer r.Close()

	srcPath, dstPath, err := copyMovePaths(r, src, dst)
	if err != nil {
		return false, err
	}
	created, err = prepareDestination(r, dstPath, options.NoOverwrite)
	if err != nil {
		return false, err
	}

	err = fs.WalkDir(r.FS(), srcPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		target := dstPath + strings.TrimPrefix(p, srcPath)
		perm := fi.Mode() & os.ModePerm

		if fi.IsDir() {
			if err := r.Mkdir(target, perm); err != nil {
				return errFromOS(err)
			}
		} else {
			if err := copyRegularFile(r, p, target, perm); err != nil {
				return err
			}
		}

		if fi.IsDir() && options.NoRecursive {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return false, errFromOS(err)
	}

	return created, nil
}

func (local LocalFileSystem) Move(ctx context.Context, src, dst string, options *MoveOptions) (created bool, err error) {
	r, err := local.openRoot()
	if err != nil {
		return false, err
	}
	defer r.Close()

	srcPath, dstPath, err := copyMovePaths(r, src, dst)
	if err != nil {
		return false, err
	}
	created, err = prepareDestination(r, dstPath, options.NoOverwrite)
	if err != nil {
		return false, err
	}

	if err := r.Rename(srcPath, dstPath); err != nil {
		return false, errFromOS(err)
	}

	return created, nil
}
