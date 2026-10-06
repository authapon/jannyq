//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// cleanPath checks a path inside a workspace: relative, without "..", no
// empty or NUL-containing parts. Symlinks are handled by os.Root, which refuses
// to follow any that lead out of the workspace.
func cleanPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || len(p) > 512 {
		return "", ErrBadPath
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrBadPath
	}
	return clean, nil
}

// withRoot runs fn with the workspace opened as an os.Root, holding the
// workspace's lock so that it cannot run at the same time as a command.
func (e *Executor) withRoot(ws string, fn func(root *os.Root, uid int) error) error {
	if !validWorkspace(ws) {
		return ErrInvalidWorkspace
	}
	if !e.lock(ws) {
		return ErrBusy
	}
	defer e.unlock(ws)
	dir, uid, err := e.prepare(ws)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root, uid)
}

// mkdirAll creates the parent directories of rel inside root, giving them to
// the workspace's user.
func (e *Executor) mkdirAll(root *os.Root, rel string, uid int) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		if err := root.Mkdir(cur, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		if uid >= 0 {
			d, err := root.Open(cur)
			if err != nil {
				return err
			}
			err = d.Chown(uid, uid)
			d.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Put implements Files.
func (e *Executor) Put(ctx context.Context, ws, rel string, r io.Reader, maxBytes int64) error {
	rel, err := cleanPath(rel)
	if err != nil {
		return err
	}
	limit := e.cfg.UploadMaxBytes
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	return e.withRoot(ws, func(root *os.Root, uid int) error {
		dirPath := e.cfg.WorkDir + "/" + ws
		used := dirSize(dirPath, e.cfg.WorkspaceQuota)
		if used >= e.cfg.WorkspaceQuota {
			return ErrQuota
		}
		if err := e.mkdirAll(root, rel, uid); err != nil {
			return mapFSError(err)
		}
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return mapFSError(err)
		}
		ok := false
		defer func() {
			f.Close()
			if !ok {
				_ = root.Remove(rel)
			}
		}()
		if uid >= 0 {
			if err := f.Chown(uid, uid); err != nil {
				return err
			}
		}
		room := e.cfg.WorkspaceQuota - used
		if room < limit {
			limit = room
		}
		n, err := io.Copy(f, io.LimitReader(&ctxReader{ctx: ctx, r: r}, limit+1))
		if err != nil {
			return err
		}
		if n > limit {
			if limit == room {
				return ErrQuota
			}
			return ErrTooLarge
		}
		ok = true
		return nil
	})
}

// Get implements Files.
func (e *Executor) Get(ctx context.Context, ws, rel string, maxBytes int64) ([]byte, error) {
	rel, err := cleanPath(rel)
	if err != nil {
		return nil, err
	}
	limit := e.cfg.UploadMaxBytes
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	var data []byte
	err = e.withRoot(ws, func(root *os.Root, _ int) error {
		f, err := root.Open(rel)
		if err != nil {
			return mapFSError(err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return ErrNotFound
		}
		if fi.Size() > limit {
			return ErrTooLarge
		}
		data, err = io.ReadAll(io.LimitReader(&ctxReader{ctx: ctx, r: f}, limit+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > limit {
			return ErrTooLarge
		}
		return nil
	})
	return data, err
}

// Remove implements Files.
func (e *Executor) Remove(_ context.Context, ws, rel string) error {
	rel, err := cleanPath(rel)
	if err != nil {
		return err
	}
	return e.withRoot(ws, func(root *os.Root, _ int) error {
		fi, err := root.Lstat(rel)
		if err != nil {
			return mapFSError(err)
		}
		if fi.IsDir() {
			return ErrBadPath // only files; Reset removes a whole workspace
		}
		return mapFSError(root.Remove(rel))
	})
}

func mapFSError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case strings.Contains(err.Error(), "path escapes"), errors.Is(err, fs.ErrInvalid):
		return ErrBadPath
	}
	return fmt.Errorf("sandbox: %w", err)
}

// ctxReader stops a long copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
