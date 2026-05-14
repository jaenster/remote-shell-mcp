package dockerx

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/moby/moby/client"
)

// Upload streams a local file or directory into a container at remotePath.
// Semantics mirror ssh_upload: remotePath is where the data lands — for a file
// that is the exact destination path; for a directory it becomes the root of
// the mirrored tree inside the container. Docker's archive API requires a tar
// stream rooted at the destination's parent directory, so we rebase entries
// here to make `local_path / remote_path` behave the obvious way.
func (h *Host) Upload(ctx context.Context, containerID, localPath, remotePath string) (int64, error) {
	c, err := h.client()
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return 0, err
	}

	dstDir := path.Dir(remotePath)
	dstBase := path.Base(remotePath)
	if dstDir == "" || dstDir == "." {
		dstDir = "/"
	}

	pr, pw := io.Pipe()
	var written int64
	go func() {
		defer pw.Close()
		tw := tar.NewWriter(pw)
		defer tw.Close()
		werr := writeTar(tw, localPath, dstBase, info, &written)
		if werr != nil {
			_ = pw.CloseWithError(werr)
		}
	}()

	if _, err := c.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: dstDir,
		Content:         pr,
	}); err != nil {
		return written, err
	}
	return written, nil
}

func writeTar(tw *tar.Writer, src, entryName string, info os.FileInfo, written *int64) error {
	if !info.IsDir() {
		return tarFile(tw, src, entryName, info, written)
	}
	// Walk the local tree, mapping every path's prefix `src` to `entryName`.
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Symlinks: skip — same policy as the SSH upload path. Following a
		// symlink could exfiltrate files from outside the source tree.
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		var name string
		switch rel {
		case ".":
			name = entryName
		default:
			name = path.Join(entryName, rel)
		}
		if fi.IsDir() {
			hdr := &tar.Header{
				Name:     name + "/",
				Mode:     int64(fi.Mode().Perm()),
				Typeflag: tar.TypeDir,
				ModTime:  fi.ModTime(),
			}
			return tw.WriteHeader(hdr)
		}
		return tarFile(tw, p, name, fi, written)
	})
}

func tarFile(tw *tar.Writer, src, name string, info os.FileInfo, written *int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
		ModTime:  info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	*written += n
	return err
}

// Download streams a file or directory out of a container to localPath.
// Semantics mirror ssh_download: for a single-file source, localPath becomes
// that file's contents; for a directory source, localPath becomes the local
// root of the mirrored tree. Docker's archive API returns a tar whose entries
// are rooted at the basename of the source path, so we strip that prefix on
// extraction to make the local layout match expectations.
func (h *Host) Download(ctx context.Context, containerID, remotePath, localPath string) (int64, error) {
	c, err := h.client()
	if err != nil {
		return 0, err
	}
	res, err := c.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{
		SourcePath: remotePath,
	})
	if err != nil {
		return 0, err
	}
	defer res.Content.Close()
	stat := res.Stat
	tr := tar.NewReader(res.Content)
	rootPrefix := path.Base(strings.TrimRight(remotePath, "/"))
	var total int64

	// Single-file case: the tar will have exactly one TypeReg entry named
	// `<basename>`. We want its content at localPath, NOT at localPath/basename
	// — so handle this with a streaming peek.
	if !stat.Mode.IsDir() {
		hdr, err := tr.Next()
		if err != nil {
			return 0, err
		}
		_ = hdr
		if dir := filepath.Dir(localPath); dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		out, err := os.Create(localPath)
		if err != nil {
			return 0, err
		}
		defer out.Close()
		n, err := io.Copy(out, tr)
		return n, err
	}

	// Directory case: extract every entry under localPath, stripping the
	// rootPrefix so the local layout mirrors the remote tree from its top.
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		return 0, err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return total, err
		}
		rel := strings.TrimPrefix(hdr.Name, rootPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue // the root directory entry itself
		}
		out := filepath.Join(localPath, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, os.FileMode(hdr.Mode).Perm()); err != nil {
				return total, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if dir := filepath.Dir(out); dir != "" {
				_ = os.MkdirAll(dir, 0o755)
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return total, err
			}
			n, copyErr := io.Copy(f, tr)
			_ = f.Close()
			total += n
			if copyErr != nil {
				return total, copyErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip — same conservative policy as the upload side; the user
			// can re-create symlinks manually if needed.
			continue
		default:
			return total, fmt.Errorf("unexpected tar entry type %d for %s", hdr.Typeflag, hdr.Name)
		}
	}
	return total, nil
}
