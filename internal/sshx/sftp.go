package sshx

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// sftpClient lazily opens a per-session SFTP client. It is dropped when the
// session reconnects or is closed; the next call rebuilds it on the new
// transport. Reconnect.Reconnect / clearClient call dropSFTP.
func (s *Session) sftpClient() (*sftp.Client, error) {
	s.sftpMu.Lock()
	defer s.sftpMu.Unlock()
	if s.sftp != nil {
		return s.sftp, nil
	}
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("open sftp: %w", err)
	}
	s.sftp = sc
	return sc, nil
}

func (s *Session) dropSFTP() {
	s.sftpMu.Lock()
	c := s.sftp
	s.sftp = nil
	s.sftpMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// closeSFTP is retained for callers that used the previous global API.
func (s *Session) closeSFTP() { s.dropSFTP() }

type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
	Target  string    `json:"target,omitempty"`
}

func (s *Session) FileList(path string) ([]FileEntry, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	entries, err := c.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		fe := FileEntry{
			Name:    e.Name(),
			Size:    e.Size(),
			Mode:    e.Mode().String(),
			ModTime: e.ModTime(),
			IsDir:   e.IsDir(),
		}
		if e.Mode()&os.ModeSymlink != 0 {
			target, _ := c.ReadLink(filepath.Join(path, e.Name()))
			fe.Target = target
		}
		out = append(out, fe)
	}
	return out, nil
}

func (s *Session) FileStat(path string) (*FileEntry, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	st, err := c.Stat(path)
	if err != nil {
		return nil, err
	}
	return &FileEntry{
		Name:    st.Name(),
		Size:    st.Size(),
		Mode:    st.Mode().String(),
		ModTime: st.ModTime(),
		IsDir:   st.IsDir(),
	}, nil
}

// maxFileReadBytes caps a single FileRead call to keep a malicious or buggy
// client from OOMing the daemon by asking for, say, 100 GiB.
const maxFileReadBytes = 64 * 1024 * 1024

func (s *Session) FileRead(path string, offset int64, length int64) ([]byte, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	f, err := c.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if length > 0 {
		if length > maxFileReadBytes {
			length = maxFileReadBytes
		}
		buf := make([]byte, length)
		n, err := io.ReadFull(f, buf)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return buf[:n], nil
		}
		if err != nil {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(io.LimitReader(f, maxFileReadBytes))
}

func (s *Session) FileWrite(path string, data []byte, append bool) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := c.OpenFile(path, flags)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (s *Session) FileDelete(path string) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Remove(path)
}

func (s *Session) FileMkdir(path string, recursive bool) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	if recursive {
		return c.MkdirAll(path)
	}
	return c.Mkdir(path)
}

func (s *Session) FileChmod(path string, mode os.FileMode) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Chmod(path, mode)
}

func (s *Session) FileRename(from, to string) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Rename(from, to)
}

// Upload streams a local file OR directory to the remote host over SFTP. For a
// directory, the tree is mirrored under remotePath with intermediate dirs
// created as needed; permission bits are preserved on a best-effort basis.
// Symlinks are skipped (not followed, not recreated) to avoid escaping the
// source tree via a hostile or accidental local link.
func (s *Session) Upload(localPath, remotePath string) (int64, error) {
	c, err := s.sftpClient()
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return uploadFile(c, localPath, remotePath, info.Mode())
	}
	if err := c.MkdirAll(remotePath); err != nil {
		return 0, err
	}
	var total int64
	walkErr := filepath.Walk(localPath, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(localPath, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		rem := remotePath
		if rel != "." {
			rem = path.Join(remotePath, rel)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return nil
		case fi.IsDir():
			return c.MkdirAll(rem)
		default:
			n, ferr := uploadFile(c, p, rem, fi.Mode())
			total += n
			return ferr
		}
	})
	return total, walkErr
}

// Download streams a remote file OR directory from the remote host over SFTP
// to the local filesystem. For a directory, the tree is mirrored under
// localPath with intermediate dirs created as needed. Symlinks are skipped.
func (s *Session) Download(remotePath, localPath string) (int64, error) {
	c, err := s.sftpClient()
	if err != nil {
		return 0, err
	}
	info, err := c.Lstat(remotePath)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return downloadFile(c, remotePath, localPath)
	}
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		return 0, err
	}
	var total int64
	walker := c.Walk(remotePath)
	for walker.Step() {
		if werr := walker.Err(); werr != nil {
			return total, werr
		}
		st := walker.Stat()
		rel := strings.TrimPrefix(walker.Path(), remotePath)
		rel = strings.TrimPrefix(rel, "/")
		loc := localPath
		if rel != "" {
			loc = filepath.Join(localPath, filepath.FromSlash(rel))
		}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			continue
		case st.IsDir():
			if err := os.MkdirAll(loc, 0o755); err != nil {
				return total, err
			}
		default:
			n, ferr := downloadFile(c, walker.Path(), loc)
			total += n
			if ferr != nil {
				return total, ferr
			}
		}
	}
	return total, nil
}

func uploadFile(c *sftp.Client, local, remote string, mode os.FileMode) (int64, error) {
	in, err := os.Open(local)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := c.Create(remote)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		return n, err
	}
	// Best-effort permission preservation.
	_ = c.Chmod(remote, mode.Perm())
	return n, nil
}

func downloadFile(c *sftp.Client, remote, local string) (int64, error) {
	if dir := filepath.Dir(local); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	in, err := c.Open(remote)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(local)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}
